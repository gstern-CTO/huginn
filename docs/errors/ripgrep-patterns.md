# INVALID_INPUT — ripgrep rejected the pattern

`local_search_code` hands your pattern to ripgrep as an argument, never through
a shell. This error means ripgrep itself could not compile it.

## The quickest fix

Set `regex: false`. The pattern is then matched literally and no character has
special meaning:

```json
{ "pattern": "map[string]any", "regex": false }
```

That resolves most failures, because the usual cause is regex metacharacters in
text that was meant literally — `[` `]` `(` `)` `*` `+` `?` `{` `}` `|` `^` `$`
`.` `\`.

## If you do want a regex

ripgrep uses Rust regex syntax. Two differences from PCRE catch people out:

- **No backreferences** — `(\w+)\s+\1` will not compile.
- **No lookaround** — `(?=…)`, `(?!…)`, `(?<=…)` are unsupported.

Both are omitted deliberately so matching stays linear-time. Restructure the
search, or filter the results afterwards.

Supported and useful: `\b` word boundaries, character classes, `+ * ? {n,m}`,
alternation, and named groups `(?P<name>…)`.

## Narrowing beats complicating

A simple pattern with a filter usually beats a clever pattern:

| Instead of | Use |
| --- | --- |
| A regex that matches only Go files | `fileTypes: ["go"]` |
| A regex that excludes vendor directories | `globs: ["!vendor/**"]` |
| A regex scoped to one directory | `path: "internal/security"` |

## Shell metacharacters are safe

`;`, `&&`, `$(…)` and backticks in a pattern are inert. Arguments are passed as
an array, so nothing is interpreted as a command — the pattern is searched for
literally as text.

## Before searching content

Set `discovery: true` for a per-file match count. It is a cheap first pass; then
re-run without it on the densest file.
