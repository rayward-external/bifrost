<!-- RAYWARD FORK ONLY — not upstream. -->
# CLAUDE.md — rayward-external/bifrost

This repository is **`rayward-external/bifrost`**, a private fork of
[`maximhq/bifrost`](https://github.com/maximhq/bifrost). See [`AGENTS.md`](AGENTS.md)
for the codebase guide; it applies to every tool.

## PRs go to THIS fork, never upstream

- **Open every pull request against `rayward-external/bifrost`**, targeting its own
  default branch (`main`). **Never open a PR against `maximhq/bifrost`.**
- Contributing a patch upstream is a **publishing decision only the repo owner makes**.
  It puts the owner's GitHub identity and this fork on a public third-party project and
  triggers CLA obligations. If a change looks worth upstreaming, recommend it and stop —
  do not open it. A line in a plan, an option description, or a
  `.github/fork-patches.txt` "REMOVAL CONDITION" that mentions upstreaming describes an
  intended end state; it is **not** authorization.
- **`gh` in this checkout silently targets UPSTREAM.** Pass
  `--repo rayward-external/bifrost` on every `gh pr` and `gh issue` command. A bare
  `gh pr create` here opens a PR against `maximhq/bifrost`, and because upstream shares
  the same PR numbering, a bare `gh pr view <n>` returns *upstream's* PR with no error.
- **Never merge a PR without explicit user confirmation** ("merge it" / "ship it").
  A prior okay does not generalize to later PRs in the session.
- **Always share the PR link** in your reply, every time, without being asked.
