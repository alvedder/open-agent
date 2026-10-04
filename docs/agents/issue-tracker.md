# Issue tracker: GitHub

Issues and specs live in [alvedder/open-agent](https://github.com/alvedder/open-agent/issues).
Use `gh` with explicit `--repo alvedder/open-agent` for issue commands. The local
`origin` remote points to upstream; the `alvedder` remote is the fork.

## Operations

- Create: `gh issue create --repo alvedder/open-agent --title "..." --body-file <file>`.
- Read: `gh issue view <number> --repo alvedder/open-agent --comments`.
- List: `gh issue list --repo alvedder/open-agent --state all --json number,title,body,labels,comments`.
- Comment: `gh issue comment <number> --repo alvedder/open-agent --body-file <file>`.
- Labels: `gh issue edit <number> --repo alvedder/open-agent --add-label "..."` or `--remove-label "..."`.
- Close completed work: `gh issue close <number> --repo alvedder/open-agent --reason completed`.

Write multiline text to a file and pass `--body-file`. Publishing to the issue
tracker means creating a GitHub issue; fetching a ticket means reading it and
its comments. Apply readiness labels using `docs/agents/triage-labels.md`.

## Parents and dependencies

Use native GitHub sub-issues for parent/child relationships and native issue
dependencies for blocking. Get numeric database IDs with
`gh api repos/alvedder/open-agent/issues/<number> --jq .id`.

- Add a child: `gh api --method POST repos/alvedder/open-agent/issues/<parent>/sub_issues -F sub_issue_id=<child-database-id>`.
- Add a blocker: `gh api --method POST repos/alvedder/open-agent/issues/<child>/dependencies/blocked_by -F issue_id=<blocker-database-id>`.

If native relationships are unavailable, put a child checklist in the parent,
`Part of #<parent>` in each child and `Blocked by: #<number>` in the blocked issue.
An issue is unblocked when all blockers are closed.

## Migrated skills work

The approved specification and public completion evidence are in
[issue #1](https://github.com/alvedder/open-agent/issues/1). Its six completed
children are #2 through #7. Private `.scratch/skills` files are an ignored
historical archive; GitHub issues are authoritative for future tracking.

## Pull requests as a triage surface

**PRs as a request surface: no.** Change to `yes` to include external PRs in triage.
GitHub issues and PRs share a number space; resolve ambiguous references with
`gh pr view <number> --repo alvedder/open-agent`, then fall back to `gh issue view`.
