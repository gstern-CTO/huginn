# INVALID_INPUT — GitHub query syntax

`github_search_code` assembles GitHub's query string for you from structured
fields, so you never write qualifiers by hand. This error means those fields
could not be assembled into a valid query, or GitHub rejected the result.

## The fields

| Field | Meaning | Example |
| --- | --- | --- |
| `keywords` | **Required.** Terms, ANDed together | `["ServeStdio"]` |
| `owner` | A user or organisation | `"mark3labs"` |
| `repo` | A repository — **requires `owner`** | `"mcp-go"` |
| `extension` | File extension, no dot | `"go"` |
| `path` | Path prefix | `"internal/"` |
| `language` | GitHub language name | `"Go"` |
| `match` | `"file"` for content (default) or `"path"` for filenames | `"path"` |

## The three ways this fails

**`each query needs at least one keyword`** — GitHub code search cannot run on
qualifiers alone. Add a term. To browse a repository without searching, use
`github_repo_structure` instead.

**`repo filter "x" requires an owner`** — `repo` takes a bare name, not
`owner/name`. Pass the two as separate fields.

**`match must be 'file' or 'path'`** — only those two values exist.

## When GitHub itself rejects the query

A 422 means the assembled query was too complex or malformed. Simplify: drop
`language` and `path` first, then `extension`, and retry with keywords plus at
most one qualifier.

## Getting better results

- **Nothing found?** Remove the extension filter first — it is the most common
  cause of an empty result. Then try `match: "path"` to search filenames.
- **Too many results?** Add `owner`, or `repo` together with its `owner`.
- **Mapping a landscape?** Set `concise: true` for paths only. Much cheaper, and
  you can read the interesting files afterwards.
- **Several angles at once?** Pass up to 5 queries in one call. They run in
  parallel, and one failing still returns the others.

## Related

`github_search_repos` searches keywords and topics as **separate** queries and
deduplicates the results, because combining them into a single GitHub query
scores poorly.
