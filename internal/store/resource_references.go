package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

const (
	resourceKindSource      = "source"
	resourceKindNode        = "node"
	resourceKindTemplate    = "template"
	resourceKindRuleProfile = "rule_profile"

	resourceDeletionProtected  = "protected"
	resourceDeletionReferenced = "referenced"
)

type resourceDeletionReference struct {
	SubscriptionID   int64  `json:"subscription_id"`
	SubscriptionName string `json:"subscription_name"`
}

// ResourceDeletionConflict describes a deletion invariant enforced by Store.
// Callers may present the references, but they must not decide whether deletion
// is allowed themselves.
type ResourceDeletionConflict struct {
	ResourceKind string                      `json:"resource_kind"`
	ResourceID   int64                       `json:"resource_id"`
	Reason       string                      `json:"reason"`
	References   []resourceDeletionReference `json:"references,omitempty"`
}

func (e *ResourceDeletionConflict) Error() string {
	resource := fmt.Sprintf("%s %d", e.ResourceKind, e.ResourceID)
	if e.Reason == resourceDeletionProtected {
		return resource + " is protected and cannot be deleted"
	}
	references := make([]string, 0, len(e.References))
	for _, reference := range e.References {
		references = append(references, fmt.Sprintf("%d (%q)", reference.SubscriptionID, reference.SubscriptionName))
	}
	return resource + " is referenced by output subscriptions: " + strings.Join(references, ", ")
}

type resourceDeletionNotFoundError struct {
	resourceKind string
	resourceID   int64
}

func (e *resourceDeletionNotFoundError) Error() string {
	return fmt.Sprintf("no %s with id %d", e.resourceKind, e.resourceID)
}

// IsResourceDeletionNotFound reports whether a Store deletion failed because
// the requested resource does not exist.
func IsResourceDeletionNotFound(err error) bool {
	var target *resourceDeletionNotFoundError
	return errors.As(err, &target)
}

type resourceReferenceGraph struct {
	subscriptions       []OutputSubscription
	sourceByNodeID      map[int64]int64
	templateByVersionID map[int64]int64
}

func loadResourceReferenceGraphTx(tx *bolt.Tx) (resourceReferenceGraph, error) {
	graph := resourceReferenceGraph{
		subscriptions:       make([]OutputSubscription, 0),
		sourceByNodeID:      make(map[int64]int64),
		templateByVersionID: make(map[int64]int64),
	}
	if err := tx.Bucket([]byte("subscriptions")).ForEach(func(_, raw []byte) error {
		var subscription OutputSubscription
		if err := json.Unmarshal(raw, &subscription); err != nil {
			return err
		}
		graph.subscriptions = append(graph.subscriptions, subscription)
		return nil
	}); err != nil {
		return resourceReferenceGraph{}, err
	}
	if err := tx.Bucket([]byte("nodes")).ForEach(func(_, raw []byte) error {
		var node NodeRecord
		if err := json.Unmarshal(raw, &node); err != nil {
			return err
		}
		graph.sourceByNodeID[node.ID] = node.SourceID
		return nil
	}); err != nil {
		return resourceReferenceGraph{}, err
	}
	if err := tx.Bucket([]byte("template_versions")).ForEach(func(_, raw []byte) error {
		var version TemplateVersion
		if err := json.Unmarshal(raw, &version); err != nil {
			return err
		}
		graph.templateByVersionID[version.ID] = version.TemplateID
		return nil
	}); err != nil {
		return resourceReferenceGraph{}, err
	}
	sort.Slice(graph.subscriptions, func(i, j int) bool {
		return graph.subscriptions[i].ID < graph.subscriptions[j].ID
	})
	return graph, nil
}

func (g resourceReferenceGraph) references(resourceKind string, resourceID int64) ([]resourceDeletionReference, error) {
	references := make([]resourceDeletionReference, 0)
	for _, subscription := range g.subscriptions {
		var referenced bool
		switch resourceKind {
		case resourceKindSource:
			referenced = subscriptionReferencesNode(subscription, func(nodeID int64) bool {
				return g.sourceByNodeID[nodeID] == resourceID
			})
		case resourceKindNode:
			referenced = subscriptionReferencesNode(subscription, func(nodeID int64) bool {
				return nodeID == resourceID
			})
		case resourceKindTemplate:
			referenced = g.templateByVersionID[subscription.TemplateVersionID] == resourceID
		case resourceKindRuleProfile:
			referenced = subscription.RuleProfileID == resourceID
		default:
			return nil, fmt.Errorf("unsupported resource kind %q", resourceKind)
		}
		if referenced {
			references = append(references, resourceDeletionReference{
				SubscriptionID:   subscription.ID,
				SubscriptionName: subscription.Name,
			})
		}
	}
	return references, nil
}

func subscriptionReferencesNode(subscription OutputSubscription, matches func(int64) bool) bool {
	for _, binding := range subscription.Bindings {
		for _, nodeID := range binding.NodeIDs {
			if matches(nodeID) {
				return true
			}
		}
	}
	return false
}

func requireResourceDeletionAllowedTx(tx *bolt.Tx, resourceKind string, resourceID int64, protected bool) error {
	if protected {
		return &ResourceDeletionConflict{
			ResourceKind: resourceKind,
			ResourceID:   resourceID,
			Reason:       resourceDeletionProtected,
		}
	}
	graph, err := loadResourceReferenceGraphTx(tx)
	if err != nil {
		return err
	}
	references, err := graph.references(resourceKind, resourceID)
	if err != nil {
		return err
	}
	if len(references) > 0 {
		return &ResourceDeletionConflict{
			ResourceKind: resourceKind,
			ResourceID:   resourceID,
			Reason:       resourceDeletionReferenced,
			References:   references,
		}
	}
	return nil
}
