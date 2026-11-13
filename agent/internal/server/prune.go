// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/prunecat"
	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// The Prune RPC generalizes PruneImages to every kind of object that collects
// on a node: stopped containers, images, build cache, networks and volumes.
// Each category is its own authorization action and its own audit record, so a
// policy can allow the cheap sweeps and refuse the dangerous ones, and the
// trail says which happened.
//
// Two of them carry guards that are the point of doing this in the agent
// rather than shelling out to `docker system prune`:
//
//   - networks: the Engine's networks/prune is never called. On a manager it
//     also removes every unused swarm-scope OVERLAY network, cluster-wide, and
//     the API cannot filter it by scope. Networks are listed, vetted (local
//     scope, not predefined, referenced by no container) and removed singly.
//   - containers: only those that FINISHED longer ago than containers_until
//     (default 24h). The cli falls back to the last exited container's logs
//     when a task crash-loops; pruning it a minute later would take the one
//     useful log away.
//
// Volumes are data. They are pruned only when asked for by name, never as part
// of a default — the schedule labels and the cli both enforce that.

// predefinedNetworks are never removed, whatever else they look like. bridge,
// host and none are the daemon's own; docker_gwbridge and ingress belong to
// swarm mode and break it when removed.
var predefinedNetworks = map[string]bool{
	"bridge": true, "host": true, "none": true, "docker_gwbridge": true, "ingress": true,
}

// pruneOptions are a run's knobs, shared by the RPC and the schedule.
type pruneOptions struct {
	containersUntil time.Duration
	labelFilters    []string
}

// Prune reclaims space on this node for the requested categories.
func (s *Server) Prune(ctx context.Context, req *pb.PruneRequest) (*pb.PruneResponse, error) {
	identity, err := s.identity(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.GetCategories()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "PruneRequest names no categories; there is no implicit \"everything\"")
	}
	for _, c := range req.GetCategories() {
		if _, ok := prunecat.Info(c); !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown prune category %v", c)
		}
	}
	opts := pruneOptions{containersUntil: prunesched.DefaultContainersUntil}
	if u := strings.TrimSpace(req.GetContainersUntil()); u != "" {
		d, err := prunesched.ParseUntil(u)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "containers_until: %v", err)
		}
		opts.containersUntil = d
	}
	if lf := req.GetLabelFilters(); len(lf) > 0 {
		f, err := prunesched.ParseLabelFilters(strings.Join(lf, ","))
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "label_filters: %v", err)
		}
		opts.labelFilters = f
	}

	results := s.runPrune(ctx, identity, "manual", req.GetCategories(), opts)
	// What was just freed is no longer reclaimable; the cached figures would
	// say otherwise for up to an hour.
	s.reclaim.Kick()
	return &pb.PruneResponse{Results: results}, nil
}

// runPrune runs the categories in prunecat.Order, each authorized and audited on
// its own. A refused or failed category does not stop the others: they are
// independent acts, and a run that removed the containers and then gave up on
// everything else would leave the most to clean up for the least gain.
func (s *Server) runPrune(ctx context.Context, identity, trigger string, cats []pb.PruneCategory, opts pruneOptions) []*pb.PruneCategoryResult {
	// One prune at a time: a manual run and the schedule overlapping would
	// race each other for the same objects and report half of them each.
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()

	want := map[pb.PruneCategory]bool{}
	for _, c := range cats {
		want[c] = true
	}
	// Every unused image includes every dangling one.
	if want[pb.PruneCategory_PRUNE_CATEGORY_IMAGES] {
		delete(want, pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES)
	}

	var out []*pb.PruneCategoryResult
	for _, pc := range prunecat.Order {
		if !want[pc.Cat] {
			continue
		}
		res := &pb.PruneCategoryResult{Category: pc.Cat}
		out = append(out, res)

		if reason, ok := s.decide(ctx, auth.Request{Action: pc.Action, Identity: identity}); !ok {
			res.Error = "authorization denied: " + reason
			s.audit.Prune(identity, trigger, pc.Name, 0, 0, false, false, res.Error)
			continue
		}

		freed, count, skipped, err := s.pruneOne(ctx, pc.Cat, opts)
		res.FreedBytes, res.Count, res.Skipped = freed, count, skipped
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
			res.Error = errMsg
			s.log.Error("prune failed", "category", pc.Name, "trigger", trigger, "err", err)
		}
		s.audit.Prune(identity, trigger, pc.Name, freed, count, skipped, err == nil, errMsg)
	}
	return out
}

// pruneOne performs one category. A partial failure still reports what it
// did free: the bytes are gone either way.
func (s *Server) pruneOne(ctx context.Context, c pb.PruneCategory, opts pruneOptions) (freed, count int64, skipped bool, err error) {
	switch c {
	case pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS:
		freed, count, err = s.pruneContainers(ctx, opts)
	case pb.PruneCategory_PRUNE_CATEGORY_IMAGES, pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES:
		f := pruneFilters(c == pb.PruneCategory_PRUNE_CATEGORY_IMAGES)
		addLabelFilters(f, opts.labelFilters)
		res, perr := s.docker.ImagePrune(ctx, client.ImagePruneOptions{Filters: f})
		if perr != nil {
			return 0, 0, false, perr
		}
		freed, count = int64(res.Report.SpaceReclaimed), int64(len(res.Report.ImagesDeleted))
	case pb.PruneCategory_PRUNE_CATEGORY_BUILD_CACHE:
		// Build cache records carry no labels; under a label filter the only
		// honest answer is "not attempted", never "all of it".
		if len(opts.labelFilters) > 0 {
			return 0, 0, true, nil
		}
		res, perr := s.docker.BuildCachePrune(ctx, client.BuildCachePruneOptions{All: true})
		if perr != nil {
			return 0, 0, false, perr
		}
		freed, count = int64(res.Report.SpaceReclaimed), int64(len(res.Report.CachesDeleted))
	case pb.PruneCategory_PRUNE_CATEGORY_NETWORKS:
		count, err = s.pruneNetworks(ctx, opts.labelFilters)
	case pb.PruneCategory_PRUNE_CATEGORY_VOLUMES:
		// all=true: since API 1.42 a volume prune without it removes only
		// ANONYMOUS volumes, which is not what "unused volumes" means to anyone
		// looking at a list of named ones.
		f := make(client.Filters)
		addLabelFilters(f, opts.labelFilters)
		res, perr := s.docker.VolumePrune(ctx, client.VolumePruneOptions{All: true, Filters: f})
		if perr != nil {
			return 0, 0, false, perr
		}
		freed, count = int64(res.Report.SpaceReclaimed), int64(len(res.Report.VolumesDeleted))
	default:
		return 0, 0, false, fmt.Errorf("unknown category %v", c)
	}
	return freed, count, false, err
}

// matchesLabels applies a label filter the way the daemon does: "key" needs
// the key, "key=value" the value as well, and every filter must hold.
func matchesLabels(have map[string]string, filters []string) bool {
	for _, f := range filters {
		k, v, withValue := strings.Cut(f, "=")
		got, ok := have[k]
		if !ok || (withValue && got != v) {
			return false
		}
	}
	return true
}

func addLabelFilters(f client.Filters, labels []string) {
	if len(labels) > 0 {
		f.Add("label", labels...)
	}
}

// stoppedContainer is a prune candidate: what it costs and when it stopped.
type stoppedContainer struct {
	id       string
	size     int64
	finished time.Time
}

// stoppedContainers lists the node's stopped containers with their writable
// layer size and the time they stopped. The list API has no finish time, so
// each one is inspected: a container's AGE is the wrong clock for "are its
// logs still interesting" — a service that ran for a month and crashed a
// minute ago is exactly the one an operator is about to look at, and Docker's
// own prune `until` filter, which counts from creation, would remove it.
func (s *Server) stoppedContainers(ctx context.Context, labels []string) ([]stoppedContainer, error) {
	f := make(client.Filters).Add("status", "exited", "created", "dead")
	addLabelFilters(f, labels)
	list, err := s.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Size: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("list stopped containers: %w", err)
	}
	var out []stoppedContainer
	for _, c := range list.Items {
		// The daemon filtered by status already; checking again costs nothing
		// and means a filter that was silently ignored cannot remove a running
		// container.
		switch c.State {
		case "exited", "created", "dead":
		default:
			continue
		}
		// The daemon applied the label filter too. Checked again here because
		// the cost of a filter it silently ignored is removing someone else's
		// containers.
		if !matchesLabels(c.Labels, labels) {
			continue
		}
		sc := stoppedContainer{id: c.ID, size: c.SizeRw, finished: time.Unix(c.Created, 0)}
		if res, ierr := s.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{}); ierr == nil && res.Container.State != nil {
			if t, perr := time.Parse(time.RFC3339Nano, res.Container.State.FinishedAt); perr == nil && t.Year() > 1 {
				sc.finished = t
			}
		}
		out = append(out, sc)
	}
	return out, nil
}

// splitByAge separates the containers a prune removes from those it keeps.
// until=0 keeps none.
func splitByAge(cs []stoppedContainer, until time.Duration, now time.Time) (prune, keep []stoppedContainer) {
	cutoff := now.Add(-until)
	for _, c := range cs {
		if until > 0 && c.finished.After(cutoff) {
			keep = append(keep, c)
			continue
		}
		prune = append(prune, c)
	}
	return prune, keep
}

func (s *Server) pruneContainers(ctx context.Context, opts pruneOptions) (freed, count int64, err error) {
	cs, err := s.stoppedContainers(ctx, opts.labelFilters)
	if err != nil {
		return 0, 0, err
	}
	victims, _ := splitByAge(cs, opts.containersUntil, time.Now())
	var errs []error
	for _, c := range victims {
		// No Force, no RemoveVolumes: a container that started again between
		// the list and now is refused by the daemon, and anonymous volumes are
		// the volumes category's business.
		if _, rerr := s.docker.ContainerRemove(ctx, c.id, client.ContainerRemoveOptions{}); rerr != nil {
			errs = append(errs, fmt.Errorf("remove container %s: %w", shortID(c.id), rerr))
			continue
		}
		freed += c.size
		count++
	}
	return freed, count, firstErr(errs)
}

// removableNetwork is the static half of the network guard: what a network IS
// decides whether it may ever be removed, before anything asks whether it is
// in use.
func removableNetwork(n network.Network) bool {
	switch {
	case n.Scope != "local": // swarm (overlay) and global networks are cluster objects
		return false
	case n.Driver == "overlay":
		return false
	case predefinedNetworks[n.Name]:
		return false
	case n.Ingress:
		return false
	case n.ConfigOnly: // the config a swarm-scope macvlan/ipvlan network is built from
		return false
	}
	return true
}

// prunableNetworks lists the local networks a prune may remove: removable by
// kind, referenced by no container — stopped ones included, which is stricter
// than the Engine, whose prune only checks live endpoints — and with no
// endpoint attached right now.
func (s *Server) prunableNetworks(ctx context.Context, labels []string) ([]network.Network, error) {
	f := make(client.Filters).Add("scope", "local")
	addLabelFilters(f, labels)
	list, err := s.docker.NetworkList(ctx, client.NetworkListOptions{Filters: f})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	cs, err := s.docker.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	used := map[string]bool{}
	for _, c := range cs.Items {
		if c.NetworkSettings == nil {
			continue
		}
		for name, ep := range c.NetworkSettings.Networks {
			used[name] = true
			if ep != nil && ep.NetworkID != "" {
				used[ep.NetworkID] = true
			}
		}
	}
	var out []network.Network
	for _, n := range list.Items {
		if !removableNetwork(n.Network) || used[n.ID] || used[n.Name] || !matchesLabels(n.Labels, labels) {
			continue
		}
		insp, ierr := s.docker.NetworkInspect(ctx, n.ID, client.NetworkInspectOptions{})
		if ierr != nil {
			continue // cannot vouch for it: leave it
		}
		if !removableNetwork(insp.Network.Network) || len(insp.Network.Containers) > 0 {
			continue
		}
		out = append(out, n.Network)
	}
	return out, nil
}

func (s *Server) pruneNetworks(ctx context.Context, labels []string) (count int64, err error) {
	nets, err := s.prunableNetworks(ctx, labels)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, n := range nets {
		if _, rerr := s.docker.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); rerr != nil {
			errs = append(errs, fmt.Errorf("remove network %s: %w", n.Name, rerr))
			continue
		}
		count++
	}
	return count, firstErr(errs)
}

// firstErr reports the first of several failures and how many there were, so
// one bad container does not bury the count of the rest.
func firstErr(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return fmt.Errorf("%w (and %d more)", errs[0], len(errs)-1)
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
