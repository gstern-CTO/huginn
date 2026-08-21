# Huginn Design Log

Feature and architecture decisions for this project, referenced as
**Design Log #N**.

Cross-project decisions live in `../../design-log/` and are referenced as
**Workspace Log #N**. The rules for both levels are in
[`../../CLAUDE.md`](../../CLAUDE.md); project-specific refinements are in
[`../CLAUDE.md`](../CLAUDE.md).

Read this index before starting a feature. Logs #1 and #2 carry constraints that
are easy to violate by accident.

## Index

| # | Title | Date | Status |
| --- | --- | --- | --- |
| [1](0001-response-envelope-and-structured-errors.md) | Response envelope and structured errors | 2026-08-20 | Implemented · retroactive |
| [2](0002-language-server-lifetime-and-symbol-location.md) | Language server lifetime and symbol location | 2026-08-20 | Implemented |

## Conventions

- Filename `NNNN-kebab-case-slug.md`, zero-padded, numbers never reused
- Structure: Background → Problem → Questions and Answers → Design →
  Implementation Plan → Examples → Trade-offs → Verification Criteria
- Everything above "Implementation Results" freezes once implementation starts.
  Deviations are appended, never edited in.
- Include real test results (X/Y passing), not intentions.

## On the retroactive entries

Log #1 is marked **retroactive**: it was written after the code it describes,
to capture reasoning that would otherwise have been lost. It is labelled rather
than back-dated, because a log's value is that it is honest about when it was
written. See Workspace Log #1, Q6.

Log #2 is not retroactive in the same sense — the design section was written
from the investigation as it happened, and the results appended after the fix
landed.
