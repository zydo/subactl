# Round 1: void

These runs are kept as a record. Their numbers must not be used: the task
scripts had defects that made every package fail in every group, so the
groups cannot be compared on completion, work lost or utilisation.

What was wrong, and what the round found anyway:

- **The story check failed whenever the browser was left running (G2).**
  The script called a Playwright method that does not exist, so with G2 on
  no package could pass, in any group. Fixed in `shot.js`.
- **Every worker ran tsc at four times the core count (G6).** Forty
  type-checks at once need more memory than the whole chamber, so no
  envelope could hold a worker. G6 now applies to a seeded share of the
  workers (`greedy.unbounded_share`).
- **Fixers were given 512MiB, which the verification they run does not
  fit in.** Raised to 1GiB.
- **A real defect in subactl's suggestions.** The peak of a lease killed at
  its memory limit is the limit itself. Counting those peaks made the
  suggestion follow the limits: with a grow rule, group C's suggestion for
  a worker climbed from 2GiB to 9.6GiB over one run, until no worker fit
  in the lead's envelope and nothing was admitted in runs 2 to 5. Only
  leases that ran to completion count now.

What the round does show, since it does not depend on those defects: in
all five runs of group A processes were left behind and the chamber's cap
was hit; in all ten runs of groups B and C neither happened.
