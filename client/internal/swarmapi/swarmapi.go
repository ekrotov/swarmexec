// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package swarmapi names the slice of the Docker client the TUI talks to the
// Swarm manager through. The UI's clusterState holds a Client rather than a
// *client.Client, so the manager side of every tab can run against a fake —
// "volumes load while one node fails mid fan-out" becomes a test, not a
// thought experiment (review Q-06). It costs nothing at runtime: the value is
// still the real *client.Client.
//
// The method set is exactly what the UI calls, directly or through the data
// helpers it hands the client to (serviceedit.go, netsec.go, inspect.go,
// nodes.go, stackfile, …) — not the hundred-method client.APIClient. Adding a
// manager call to the UI means adding its method here, which is the point:
// the dependency is visible. The narrower interfaces the cli package already
// declares for single helpers (serviceLister, stackAPI, …) are subsets of it
// and stay where they are; they describe what those helpers need.
//
// It is meant to grow into the swarmops package of review A-01.
package swarmapi

import (
	"context"

	"github.com/moby/moby/client"
)

// Client is the Docker manager API as the TUI uses it.
type Client interface {
	// System
	Ping(context.Context, client.PingOptions) (client.PingResult, error)
	Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error)
	SwarmInspect(context.Context, client.SwarmInspectOptions) (client.SwarmInspectResult, error)
	Events(context.Context, client.EventsListOptions) client.EventsResult
	Close() error

	// Services and tasks
	ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error)
	ServiceInspect(context.Context, string, client.ServiceInspectOptions) (client.ServiceInspectResult, error)
	ServiceCreate(context.Context, client.ServiceCreateOptions) (client.ServiceCreateResult, error)
	ServiceUpdate(context.Context, string, client.ServiceUpdateOptions) (client.ServiceUpdateResult, error)
	ServiceRemove(context.Context, string, client.ServiceRemoveOptions) (client.ServiceRemoveResult, error)
	TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error)
	TaskInspect(context.Context, string, client.TaskInspectOptions) (client.TaskInspectResult, error)

	// Nodes
	NodeList(context.Context, client.NodeListOptions) (client.NodeListResult, error)
	NodeInspect(context.Context, string, client.NodeInspectOptions) (client.NodeInspectResult, error)
	NodeUpdate(context.Context, string, client.NodeUpdateOptions) (client.NodeUpdateResult, error)

	// Networks
	NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkCreate(context.Context, string, client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkRemove(context.Context, string, client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)

	// Secrets and configs
	SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error)
	SecretCreate(context.Context, client.SecretCreateOptions) (client.SecretCreateResult, error)
	SecretRemove(context.Context, string, client.SecretRemoveOptions) (client.SecretRemoveResult, error)
	ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error)
	ConfigInspect(context.Context, string, client.ConfigInspectOptions) (client.ConfigInspectResult, error)
	ConfigCreate(context.Context, client.ConfigCreateOptions) (client.ConfigCreateResult, error)
	ConfigRemove(context.Context, string, client.ConfigRemoveOptions) (client.ConfigRemoveResult, error)
}

// The real client is a Client; a method signature drifting in a moby upgrade
// fails here, at compile time, rather than at the UI's assignment.
var _ Client = (*client.Client)(nil)
