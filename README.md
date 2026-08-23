# ajent

[![license](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/jentfoo/ajent/blob/main/LICENSE)
[![Tests - Main Push](https://github.com/jentfoo/ajent/actions/workflows/tests-main.yml/badge.svg)](https://github.com/jentfoo/ajent/actions/workflows/tests-main.yml)
[![Vibe-Scale 4.0(V2|U1|T2): Significant AI, test gaps](https://img.shields.io/badge/Vibe--Scale%204.0(V2%7CU1%7CT2)-Significant%20AI%2C%20test%20gaps-ff7f0e)](https://github.com/vibesdk/vibe-scale/blob/main/scale/vibe-4.md#v2-u1-t2-score-40--significant-ai-test-gaps)

A very lightweight, minimal CLI coding agent interface written in Go.

<img width="640" height="380" alt="demo" src="https://github.com/user-attachments/assets/629b7935-b2d8-46c5-b0d5-2f9c62220e89" />

The project is deliberately opinionated. It trims the UI to maximize working space and makes firm choices about things like sub-agents and tool barriers. Those opinions are an ongoing exploration of CLI agent ergonomics, so expect them to shift as the project evolves.

Feedback is welcome. Please open issues if you have any suggestions or comments.

## Install

```sh
go install github.com/jentfoo/ajent@latest
```
## Design Philosophy

### TUI

One of the hardest things working with coding agents is needing to copy content out of the terminal. The output is either damaged, or padded and suffixed with spaces.

Our agent simplifies the TUI in order to provide a terminal native output. Everything above the prompt bar is permanent and will never be changed. It is only the lower section of the agent window which is dynamic, resizing for prompts and showing the activity of sub-agents or other tool calls.

<img width="640" height="309" alt="tools demo" src="https://github.com/user-attachments/assets/e17b834c-f3e9-407d-99d1-b870e9fc0d0c" />

### Tool Barriers

Our CLI agent attempts to balance autonomy and safety. We do this through a variety of permission methods:

* allow-reads (default) - Will automatically allow any read only tools or MCP tools which are marked as ready only in their configuration. Any write or questionable bash operations will require user approval first.
* auto - Still attempts to provide a read only experience by default, but will delegate to the agent to make decisions on bash commands which are not automatically allowed.
* auto+mcp - In addition to the above MCP tools not marked as read only will be evaluated if the specific operation is read only.
* allow-write - All operations allowed without human approval.

When a write operation does need approval, you're presented with a dialog that lets you steer, allow once, or allow for the session (or until the barrier mode is changed).

<img width="639" height="164" alt="permission-check" src="https://github.com/user-attachments/assets/8b81665a-f97c-4681-a099-7c4c26c5c718" />

### Sub-agents

Sub-agents only function in a read only form. They exist only to keep the main context free concise, offloading the exploratory research to come back with a targeted summary for the main context history. They are enabled by default, and also gain access to any `readOnly` MCP services configured.

### Tools

MCP and other tools are loaded on the first message. Once a tool is loaded it can't be unloaded, however we do allow adding tools later at the cost of a cache miss.

## Configuration

### Models

Our json model configuration is generally a superset of pi model configurations.

