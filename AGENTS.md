## Git Boundaries (critical)

- Git operations are only performed when the user explicitly issues them:
  `git commit`, `git push`, branch creation/deletion, PR creation/merging,
  remote or tag mutations, and any deployment promotion.
- Editing a file in the working tree is not a git operation and needs no
  permission. Committing or pushing that edit does.
- When in doubt, make the file change, show what changed, and ask before
  running any git command.

## PR Communication (critical)

- PRs are authored by the user's account. Write PR titles and bodies as clean,
  professional release notes from the account owner: what changed and why.
- No meta-commentary, no instructions to the operator, no first/second-person
  notes about deployment process or review flow.

## Deployment Boundaries (critical)

- The default stopping point is `main`. Do not push, merge, or deploy beyond
  `main` without an explicit user request for that exact action.
- **Never promote to `production` (or any deployed environment) without
  explicit, separate user approval.** "Merge it", "push and merge", or green
  CI are not approval to deploy.
- Never treat production as a dev/staging environment: no speculative deploys,
  no auto-promotion inside a merge pipeline, no iterative prod debugging that
  could have been surfaced in-product.
- When asked to "merge", merge to `main` only, then ask whether production
  promotion is wanted.
- Migrations run against production at deploy time. Before any deploy, confirm
  rollout intent explicitly; `migrations/` changes are part of that review.

## Agent Guidelines

### 1. Think Before Coding
Don't assume. Don't hide confusion. Surface tradeoffs.

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them — don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

### 2. Simplicity First
Minimum code that solves the problem. Nothing speculative.

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.
- Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

### 3. Surgical Changes
Touch only what you must. Clean up only your own mess.

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it — don't delete it.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.
- The test: Every changed line should trace directly to the user's request.

### 4. Goal-Driven Execution
Define success criteria. Loop until verified.

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"

For multi-step tasks, state a brief plan:
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

