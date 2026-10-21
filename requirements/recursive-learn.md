Product Requirements Document (PRD)
Project Title
Self-Improving & Token-Optimized LLM Execution Harness
---
1. Executive Summary & Core Objective
1.1 Overview
Build a standalone Python-based execution harness for Large Language Models (LLMs) that continuously improves its own execution quality over time and minimizes token consumption across multi-turn sessions. The system operates as an intelligent proxy between user applications and target LLM providers (e.g., OpenAI, Anthropic, Ollama).
1.2 Key Objectives
Recursive Self-Improvement: Automatically detect runtime task failures, execute self-correction loops, and distill systemic heuristics into a persistent vector memory without model fine-tuning.
Context & Token Optimization: Maintain sub-$O(N)$ context scaling through a dual-buffer history architecture and delta-state compression.
Model & Tool Agnostic: Provide unified abstraction wrappers for LLM providers and local code execution runtimes.
---
2. Architectural Overview
```
                      ┌─────────────────────────────────────────┐
                      │             User Application            │
                      └────────────────────┬────────────────────┘
                                           │
                                           ▼
                      ┌─────────────────────────────────────────┐
                      │          Harness Interceptor            │
                      └────────────────────┬────────────────────┘
                                           │
          ┌────────────────────────────────┴────────────────────────────────┐
          ▼                                                                 ▼
┌──────────────────┐                                            ┌──────────────────────┐
│ Dynamic Context  │                                            │  Execution & Retry   │
│     Builder      │                                            │       Engine         │
└─────────┬────────┘                                            └──────────┬───────────┘
          │                                                                │
          ├─────────────────────────┐                            ┌─────────┴───────────┐
          ▼                         ▼                            ▼                     ▼
┌──────────────────┐      ┌──────────────────┐        ┌──────────────────┐   ┌─────────────────┐
│ Vector Memory    │      │  State Buffer    │        │ Target LLM API   │   │ Local Execution │
│ (Learned Rules)  │      │ (Working Memory) │        │ (OpenAI/Claude)  │   │ Sandbox (Code)  │
└──────────────────┘      └──────────────────┘        └──────────────────┘   └─────────────────┘
                                                                                       │
                                                                                       ▼
                                                                             ┌─────────────────┐
                                                                             │ Distillation    │
                                                                             │ Worker (Async)  │
                                                                             └─────────────────┘
```
---
3. System Architecture & Functional Requirements
Module 1: Provider & Execution Layer (`harness/core/`)
`LLMClient` Abstraction:
Must implement a unified interface supporting streaming and non-streaming responses.
Native integrations: OpenAI, Anthropic, LiteLLM (for local model fallbacks).
`ExecutionSandbox` Interface:
Executable callback engine that takes standard model outputs (Python, Bash, JSON) and executes them in an isolated process or subprocess.
Must return structured execution feedback: `status` (`SUCCESS` | `FAILURE`), `stdout`, `stderr`, `returncode`.
Module 2: Token-Optimized Context Manager (`harness/context/`)
Dual-Buffer Strategy:
Working Memory Buffer: Holds raw dialogue exchanges for the last $N$ turns (configurable, default $N=2$).
Episodic State Summary: Holds a running, structured summary (`<context_summary>`) of all turns older than $N$.
Context Assembly Pipeline:
Assemble the prompt in the exact priority hierarchy:
System Base Instruction
Dynamic Learned Heuristics (`<learned_heuristics>`)
Episodic Summary (`<context_summary>`)
Working Memory (Last $N$ raw exchanges)
Current User Input
State Compression Trigger:
When `len(working_memory) > max_recent_turns * 2`, trigger a lightweight compression pass to merge the oldest pair into the `<context_summary>` state block and remove them from working memory.
Module 3: Recursive Learning Loop (`harness/learning/`)
Runtime Verification & Correction:
When `ExecutionSandbox` returns `FAILURE`, trigger an inline self-correction loop ($Max Retries \le 3$).
The correction turn must include the original task, generated code, and precise execution error trace.
Asynchronous Distillation:
Upon a successful execution that required $\ge 1$ retry, trigger an out-of-band distillation call.
Distillation prompt must convert raw errors into generalized, reusable anti-patterns and rules.
Required output format for distilled heuristics:
    ```json
    {
      "domain": "string",
      "keywords": ["string"],
      "rule": "string",
      "anti_pattern": "string"
    }
    ```
Memory & Retrieval:
Store distilled rules in a local vector DB (e.g., ChromaDB, LanceDB, or SQLite-VSS).
On every user query, perform semantic retrieval (top $K=3$) using query embeddings to fetch relevant rules and dynamically inject them into `<learned_heuristics>`.
---
4. Technical Constraints & Non-Functional Requirements
Metric / Aspect	Target Constraint
Language & Runtime	Python 3.11+
Context Overhead	Maximum 30% of total window for persistent state & heuristics combined.
Storage Footprint	Embedded vector DB preferred (no external server required by default).
Error Handling	Graceful degradation if vector search or distillation fails; core inference must never crash due to memory component failure.
Dependencies	Minimal dependencies (`pydantic`, `httpx`, `chromadb` / `lancedb`).
---
5. Directory & File Structure
```text
harness/
│
├── core/
│   ├── __init__.py
│   ├── client.py         # LLM provider wrappers
│   └── sandbox.py        # Code/Task execution environment
│
├── context/
│   ├── __init__.py
│   ├── buffer.py         # Dual-buffer working memory & compressor
│   └── builder.py        # Dynamic prompt payload assembler
│
├── learning/
│   ├── __init__.py
│   ├── distillation.py   # Rule extraction from failures
│   └── store.py          # Vector store operations for heuristics
│
├── harness.py            # Main entry point orchestrating execution turns
└── config.py             # Configuration and environment management
```
---
6. Acceptance Criteria & Definition of Done (DoD)
[ ] Multi-turn Efficiency Pass: The context size in tokens remains stable during conversations exceeding 10 turns (verify sliding window + summary compression).
[ ] Self-Correction Verification: Failing code samples automatically trigger retry cycles with execution logs included in the prompt.
[ ] Persistent Learning Test:
Step A: Submit a task designed to cause a known framework issue.
Step B: Let harness self-correct and distill the error into memory.
Step C: Reset conversation state and resubmit a similar task.
Step D: Confirm the distilled heuristic is injected into `<learned_heuristics>` and the error is avoided on turn 1.
