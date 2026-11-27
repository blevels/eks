---
name: Implementation quality
description: User's standard for simplicity and idiomatic Go in this project
---

Use the shortest, straight-line path to accomplish the objective. The solution
must be easy to follow, idiomatic Go, and the most reasonable solution to the
problem. Reducing line count is not a success criterion; even 8,000 lines is
acceptable if those standards are met.

**Why:** The user explicitly clarified that the concern is unnecessary
complexity and unfamiliar, overwrought code, not a numerical size limit.

**How to apply:** Evaluate structure by whether it makes the solution easier to
follow. Do not justify abstraction solely by testability, shorter functions,
passing checks, or an architecture label.

Production function comments must explain what the function does, the meaning
of its inputs, and its return values, including error behavior.

**Why:** The user found the existing non-test code sparsely commented and its
comments insufficiently helpful, and explicitly stated this minimum expectation.

**How to apply:** Document exported and unexported production functions in
concise Go doc comments. Explain relevant side effects and constraints; do not
substitute restating a function name or its type signature for useful explanation.
