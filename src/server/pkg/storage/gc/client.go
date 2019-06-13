package gc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/chunk"
)

type Reference struct {
	sourcetype string
	source     string
	chunk      chunk.Chunk
}

type Client interface {
	ReserveChunks(context.Context, string, []chunk.Chunk) error
	UpdateReferences(context.Context, []Reference, []Reference, string) error
}

type ClientImpl struct {
	server Server
	db     *sql.DB
}

func initializeDb(ctx context.Context, db *sql.DB) error {
	// TODO: move initialization somewhere more consistent
	_, err := db.ExecContext(ctx, `
do $$ begin
 create type reftype as enum ('chunk', 'job', 'semantic');
exception
 when duplicate_object then null;
end $$
  `)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
create table if not exists refs (
 sourcetype reftype not null,
 source text not null,
 chunk text not null,
 primary key(sourcetype, source, chunk)
)`)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
create table if not exists chunks (
 chunk text primary key,
 deleting timestamp
)`)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
create index if not exists idx_chunk on refs (chunk)
`)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
create index if not exists idx_sourcetype_source on refs (sourcetype, source)
`)
	if err != nil {
		return err
	}

	return nil
}

func MakeClient(ctx context.Context, server Server, host string, port int16) (Client, error) {
	connStr := fmt.Sprintf("host=%s port=%d dbname=pgc user=pachyderm password=elephantastic sslmode=disable", host, port)
	connector, err := pq.NewConnector(connStr)
	if err != nil {
		return nil, err
	}

	// Opening a connection is done lazily, initialization will connect
	db := sql.OpenDB(connector)

	err = initializeDb(ctx, db)
	if err != nil {
		return nil, err
	}

	return &ClientImpl{
		db:     db,
		server: server,
	}, nil
}

func readChunksFromCursor(cursor *sql.Rows) []chunk.Chunk {
	chunks := []chunk.Chunk{}
	for cursor.Next() {
		var hash string
		if err := cursor.Scan(&hash); err != nil {
			return nil
		}
		chunks = append(chunks, chunk.Chunk{Hash: hash})
	}
	return chunks
}

func isRetriableError(err error) bool {
	if err, ok := err.(*pq.Error); ok {
		name := err.Code.Class().Name()
		fmt.Printf("pq error: %v, %v\n", name, err.Error())
		return name == "transaction_rollback"
	}
	return false
}

func (gcc *ClientImpl) ReserveChunks(ctx context.Context, job string, chunks []chunk.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	questions := []string{}
	for i := 0; i < len(chunks); i++ {
		questions = append(questions, fmt.Sprintf("($%d)", i+2))
	}

	query := `
with
added_chunks as (
 insert into chunks (chunk)
  values ` + strings.Join(questions, ",") + `
 on conflict (chunk) do update set chunk = excluded.chunk
 returning chunk, deleting
),
added_refs as (
 insert into refs (chunk, source, sourcetype)
  select
   chunk, $1, 'job'::reftype
  from added_chunks where
   deleting is null
)

select chunk from added_chunks where deleting is not null;
	`

	parameters := []interface{}{job}
	for _, chunk := range chunks {
		parameters = append(parameters, chunk.Hash)
	}

	var chunksToFlush []chunk.Chunk
	for {
		txn, err := gcc.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return err
		}

		cursor, err := txn.QueryContext(ctx, query, parameters...)
		if err != nil {
			if err := txn.Rollback(); err != nil {
				return err
			}
			if isRetriableError(err) {
				continue
			}
			return err
		}

		// Flush returned chunks through the server
		chunksToFlush = readChunksFromCursor(cursor)
		cursor.Close()
		if err := cursor.Err(); err != nil {
			if err := txn.Rollback(); err != nil {
				return err
			}
			if isRetriableError(err) {
				continue
			}
			return err
		}

		if err := txn.Commit(); err != nil {
			if isRetriableError(err) {
				continue
			}
			return err
		}
		break
	}

	return gcc.server.FlushDeletes(ctx, chunksToFlush)
}

func (gcc *ClientImpl) UpdateReferences(ctx context.Context, add []Reference, remove []Reference, releaseJob string) error {
	var removeStr string
	if len(remove) == 0 {
		removeStr = "null"
	} else {
		removes := []string{}
		for _, ref := range remove {
			removes = append(removes, fmt.Sprintf("('%s', '%s', '%s')", ref.sourcetype, ref.source, ref.chunk.Hash))
		}
		removeStr = strings.Join(removes, ",")
	}

	var addStr string
	if len(add) == 0 {
		addStr = ""
	} else {
		adds := []string{}
		for _, ref := range add {
			adds = append(adds, fmt.Sprintf("('%s', '%s', '%s')", ref.sourcetype, ref.source, ref.chunk.Hash))
		}
		addStr = `
added_refs as (
 insert into refs (sourcetype, source, chunk) values ` + strings.Join(adds, ",") + `
 on conflict do nothing
),
		`
	}

	var jobStr string
	if releaseJob == "" {
		jobStr = "null"
	} else {
		jobStr = fmt.Sprintf("('job', '%s')", releaseJob)
	}

	query := `
with
` + addStr + `
del_refs as (
 delete from refs where
  (sourcetype, source, chunk) in (` + removeStr + `) or
	(sourcetype, source) in (` + jobStr + `)
 returning chunk
),
counts as (
 select chunk, count(*) - 1 as count from refs join del_refs using (chunk) group by 1
)

update chunks set
 deleting = now()
from counts where
 counts.chunk = chunks.chunk and
 count = 0
returning chunks.chunk;
	`

	// TODO: check for conflict errors and retry in a loop
	var chunksToDelete []chunk.Chunk
	for {
		txn, err := gcc.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return err
		}

		cursor, err := txn.QueryContext(ctx, query)
		if err != nil {
			if err := txn.Rollback(); err != nil {
				return err
			}
			if isRetriableError(err) {
				continue
			}
			return err
		}

		chunksToDelete = readChunksFromCursor(cursor)
		cursor.Close()
		if err := cursor.Err(); err != nil {
			if err := txn.Rollback(); err != nil {
				return err
			}
			if isRetriableError(err) {
				continue
			}
			return err
		}

		if err := txn.Commit(); err != nil {
			if isRetriableError(err) {
				continue
			}
			return err
		}
		break
	}

	return gcc.server.DeleteChunks(ctx, chunksToDelete)
}