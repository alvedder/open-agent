# Domain docs

This repository uses a single context: root `GLOSSARY.md` and `docs/adr/`.

Before exploring domain behavior, read the glossary and ADRs relevant to the
area. If these files are absent, proceed silently. Domain-modeling work creates
them lazily when terms or decisions are resolved.

Use the glossary's terms in issues, tests and design proposals. If a concept is
missing, note the gap for domain modeling. Surface a conflicting ADR explicitly
instead of silently replacing its decision.

The migrated skills specification in GitHub issue #1 also contains the approved
skills vocabulary and architecture decision. Read it when changing skills
behavior. Older `.scratch/skills` documents remain an ignored historical archive.
