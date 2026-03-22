//go:build integration

package integration

// NOTE: Agent worker integration tests require additional infrastructure
// that is not yet available in the OSS router test environment:
//
// - CreateAgentKey() method on WSHandler
// - Agent authentication and registration in the router
// - Agent job dispatch mechanism
//
// These tests should be added once the full agent worker infrastructure
// is implemented in the OSS router (currently only in the commercial server).
//
// Proposed tests:
// 1. TestAgentWorkerIntegration: connect → hello handshake → receive job → send tokens → complete
// 2. TestFullRoutingRoundTrip: HTTP request → queue → GPU worker → token stream → HTTP response (already exists in openai_test.go)
// 3. TestAgentWorkerToolExecution: agent calling external tools during job execution
//
// For now, the agent worker unit tests in internal/agentworker/worker_test.go
// provide coverage of the core worker logic.
