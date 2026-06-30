# CLAUDE.md

Guidance for Claude Code (and other AI agents) when working in this repository.

## Project Overview

- This is a **vm-replication** project (block-level server migration).
- A console-controlled replication from any platform (cloud or on-prem) to **Akamai Cloud**.
- An agent-based replication from source to destination.

## Code Conventions

- Use **Go**.
- Include comments in the code.

## Behavioral Rules

- Always write tests before implementing a feature.
- Always check whether there is an existing pull request: if there is, push the
  latest code into it and update its description; if there is no open pull
  request, create a new pull request and push the latest code into it.
- Don't merge any pull request without asking first.
- Always check for existing utility functions before writing new ones.
- When uncertain, ask — don't guess.
- If there is any code update to the console, update the documentation
  (`CONSOLE.md`) as well.
