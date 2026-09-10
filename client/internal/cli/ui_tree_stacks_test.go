package cli

import (
	"testing"

	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
)

func TestAnyStackedAndStackNameOf(t *testing.T) {
	if anyStacked(nil) {
		t.Error("anyStacked(nil) = true, want false")
	}
	loose := []resolve.Service{{Name: "a"}, {Name: "b", Stack: "  "}}
	if anyStacked(loose) {
		t.Error("anyStacked with only blank labels = true, want false — grouping would add a single pointless parent")
	}
	if !anyStacked([]resolve.Service{{Name: "a"}, {Name: "b", Stack: "shop"}}) {
		t.Error("anyStacked with one labelled service = false, want true")
	}

	if got := stackNameOf(resolve.Service{Stack: " shop "}); got != "shop" {
		t.Errorf("stackNameOf trimmed = %q, want shop", got)
	}
	if got := stackNameOf(resolve.Service{}); got != noStackLabel {
		t.Errorf("stackNameOf unlabelled = %q, want %q", got, noStackLabel)
	}
}

// Node kind must come from the reference, never from depth — a stack node used
// to satisfy isServiceNode (which returned "anything that is not a container"),
// which would have made the fold keys treat a stack as a service.
func TestNodeKindIsByReference(t *testing.T) {
	stack := tview.NewTreeNode("shop").SetReference(stackRef{name: "shop"})
	svc := tview.NewTreeNode("web").SetReference(svcRef{name: "web"})
	leaf := tview.NewTreeNode("abc").SetReference(resolve.Candidate{ContainerID: "abc"})
	placeholder := tview.NewTreeNode("(no services)") // no reference

	for _, c := range []struct {
		name          string
		n             *tview.TreeNode
		isSvc, isStck bool
	}{
		{"stack", stack, false, true},
		{"service", svc, true, false},
		{"container", leaf, false, false},
		{"placeholder", placeholder, false, false},
	} {
		if got := isServiceNode(c.n); got != c.isSvc {
			t.Errorf("%s: isServiceNode = %v, want %v", c.name, got, c.isSvc)
		}
		if got := isStackNode(c.n); got != c.isStck {
			t.Errorf("%s: isStackNode = %v, want %v", c.name, got, c.isStck)
		}
	}
}

// eachServiceNode and parentOf must work at either depth, since the tree is two
// levels when ungrouped and three when grouped.
func TestTreeWalkHelpersAtBothDepths(t *testing.T) {
	// Grouped: root → stack → service → container
	root := tview.NewTreeNode("root")
	stack := tview.NewTreeNode("shop").SetReference(stackRef{name: "shop"})
	svc := tview.NewTreeNode("web").SetReference(svcRef{name: "web"})
	leaf := tview.NewTreeNode("c1").SetReference(resolve.Candidate{ContainerID: "c1"})
	svc.AddChild(leaf)
	stack.AddChild(svc)
	root.AddChild(stack)

	var seen []string
	eachServiceNode(root, func(n *tview.TreeNode) {
		seen = append(seen, n.GetReference().(svcRef).name)
	})
	if len(seen) != 1 || seen[0] != "web" {
		t.Errorf("grouped: eachServiceNode saw %v, want [web]", seen)
	}
	if p := parentOf(root, leaf); p != svc {
		t.Errorf("grouped: parentOf(leaf) = %v, want the service node", p)
	}
	if p := parentOf(root, svc); p != stack {
		t.Errorf("grouped: parentOf(service) = %v, want the stack node", p)
	}
	if got := serviceParent(root, leaf); got != svc {
		t.Errorf("grouped: serviceParent(leaf) = %v, want the service node", got)
	}
	// A service's parent is a stack, not a service — serviceParent must say no.
	if got := serviceParent(root, svc); got != nil {
		t.Errorf("grouped: serviceParent(service) = %v, want nil", got)
	}

	// Ungrouped: root → service → container
	flat := tview.NewTreeNode("root")
	fsvc := tview.NewTreeNode("web").SetReference(svcRef{name: "web"})
	fleaf := tview.NewTreeNode("c1").SetReference(resolve.Candidate{ContainerID: "c1"})
	fsvc.AddChild(fleaf)
	flat.AddChild(fsvc)

	seen = nil
	eachServiceNode(flat, func(n *tview.TreeNode) {
		seen = append(seen, n.GetReference().(svcRef).name)
	})
	if len(seen) != 1 || seen[0] != "web" {
		t.Errorf("flat: eachServiceNode saw %v, want [web]", seen)
	}
	if got := serviceParent(flat, fleaf); got != fsvc {
		t.Errorf("flat: serviceParent(leaf) = %v, want the service node", got)
	}
}
