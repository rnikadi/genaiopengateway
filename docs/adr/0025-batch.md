# 0025: Batch through gateway-checked files, charged once when a batch finishes

- Status: accepted
- Date: 2026-10-24

## Context

OpenAI's Batch API runs a JSONL file of requests offline at about half price. A gateway that does not front it lets teams skip model allow lists, guardrails, budgets and usage records for their largest workloads. Files carry no model at the top level, batches run for up to 24 hours, and usage is known only when a batch ends. Ram asked for the gateway and the admin UI to grow together.

## Decision

- Serve `/v1/files` (`purpose=batch` only) and `/v1/batches` in OpenAI's shape on `openai` providers. Native cloud batch jobs, which work from storage buckets, come later.
- Check the whole file at upload: one model and one endpoint for every line, the caller's model allow list, and input guardrails on each line. Route by that model, rewrite each line to the chosen target's upstream model, and keep the file and its batches on that target.
- Refuse uploads from callers with output guardrails, since results come back as a file the gateway does not check.
- Record files and batches in the store with their owner, as for video jobs, so only the owning team reaches them.
- Charge a batch once, at its first terminal status, from the usage the batch reports, with a per-target `batch_discount`. A claim in the store makes the charge happen once across replicas. A background refresh finds finished batches that no client polled.
- Show batches and video jobs on a Jobs page in the admin UI, with cancel for operators who can write to the organization.

## Consequences

- An OpenAI SDK's batch code works unchanged with a gateway key, and batch spend appears in usage, budgets and alerts.
- Uploads are held in memory up to `limits.max_batch_file_bytes`.
- Budgets hold back new batches, not running ones; one batch can overshoot a budget.
- Output guardrails and batch are exclusive for now.
