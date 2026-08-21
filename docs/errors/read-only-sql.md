# FORBIDDEN_SQL — what a read-only query may contain

`databricks_query` refuses anything that could change state, **before the
statement leaves the process**. The check is not delegated to a read-only
credential or to Databricks itself.

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

## Environment

Queries run against **dev** unless you pass `env: "prod"` explicitly. If a table
looks empty, check you are querying the environment that holds the data before
concluding the query is wrong.
