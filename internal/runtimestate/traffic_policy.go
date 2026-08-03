package runtimestate

import (
	"errors"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
)

var trafficPolicyKey = []byte("traffic_policy")

func (s *Store) TrafficPolicy() (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("Runtime state is not open")
	}
	selection := runtimeapi.TrafficPolicyFollowSource
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		if value := string(metadata.Get(trafficPolicyKey)); value != "" {
			selection = value
		}
		if !validTrafficPolicy(selection) {
			return errors.New("Runtime traffic policy is invalid")
		}
		return nil
	})
	return selection, err
}

func (s *Store) SetTrafficPolicy(selection string, operationID string, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	if !validTrafficPolicy(selection) {
		return errors.New("Runtime traffic policy is invalid")
	}
	now = now.UTC()
	return s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		var err error
		if selection == runtimeapi.TrafficPolicyFollowSource {
			err = metadata.Delete(trafficPolicyKey)
		} else {
			err = metadata.Put(trafficPolicyKey, []byte(selection))
		}
		if err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "traffic_policy.updated", operationID, now)
		return err
	})
}

func trafficPolicySummary(transaction *bbolt.Tx) (runtimeapi.TrafficPolicyStatus, error) {
	metadata := transaction.Bucket(metadataBucket)
	if metadata == nil {
		return runtimeapi.TrafficPolicyStatus{}, errors.New("Runtime metadata is unavailable")
	}
	selection := string(metadata.Get(trafficPolicyKey))
	origin := runtimeapi.TrafficPolicyOriginRuntime
	if selection == "" {
		selection = runtimeapi.TrafficPolicyFollowSource
		origin = runtimeapi.TrafficPolicyOriginSource
	}
	if !validTrafficPolicy(selection) {
		return runtimeapi.TrafficPolicyStatus{}, errors.New("Runtime traffic policy is invalid")
	}
	return runtimeapi.TrafficPolicyStatus{Selection: selection, FieldOrigin: origin}, nil
}

func validTrafficPolicy(selection string) bool {
	switch selection {
	case runtimeapi.TrafficPolicyFollowSource,
		runtimeapi.TrafficPolicyRule,
		runtimeapi.TrafficPolicyGlobal,
		runtimeapi.TrafficPolicyDirect:
		return true
	default:
		return false
	}
}
