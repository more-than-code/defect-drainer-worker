package workerproto

// Project keys (worker protocol v1 §18). A Defect Drainer "app" is now a
// "project": JobSpec.projectId and JobSpec.project, and project_id on the
// spec's defects, batch and test run. Additive to v1: the old keys (appId,
// app, app_id) stay accepted until 1.0.

// CapProjectKeysV1: the worker reads and sends the project keys. A server
// adds the old keys only for a worker without it.
const CapProjectKeysV1 = "project_keys.v1"
