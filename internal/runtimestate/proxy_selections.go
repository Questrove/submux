package runtimestate

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.etcd.io/bbolt"
)

var proxySelectionsBucket = []byte("runtime_proxy_selections")

type ProxySelectionRecord struct {
	SourceID  string    `json:"source_id"`
	Group     string    `json:"group"`
	Node      string    `json:"node"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) ProxySelections(sourceID string) ([]ProxySelectionRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("Runtime state is not open")
	}
	if !validSourceID(sourceID) {
		return nil, errors.New("Runtime source ID is invalid")
	}
	prefix := []byte(sourceID + "\x00")
	result := make([]ProxySelectionRecord, 0)
	err := s.db.View(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(proxySelectionsBucket)
		cursor := bucket.Cursor()
		for key, value := cursor.Seek(prefix); key != nil && strings.HasPrefix(string(key), string(prefix)); key, value = cursor.Next() {
			var record ProxySelectionRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return errors.New("Runtime proxy selection record is invalid")
			}
			if record.SourceID != sourceID || !validProxySelectionName(record.Group) || !validProxySelectionName(record.Node) {
				return errors.New("Runtime proxy selection record is invalid")
			}
			result = append(result, record)
		}
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Group < result[j].Group })
	return result, err
}

func (s *Store) SetProxySelection(sourceID, group, node, operationID string, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	if !validSourceID(sourceID) || !validProxySelectionName(group) || !validProxySelectionName(node) {
		return errors.New("Runtime proxy selection is invalid")
	}
	record := ProxySelectionRecord{SourceID: sourceID, Group: group, Node: node, UpdatedAt: now.UTC()}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.db.Update(func(transaction *bbolt.Tx) error {
		if transaction.Bucket(sourcesBucket).Get([]byte(sourceID)) == nil {
			return ErrSourceNotFound
		}
		if err := transaction.Bucket(proxySelectionsBucket).Put([]byte(sourceID+"\x00"+group), body); err != nil {
			return err
		}
		_, err := advanceRevisionAndEvent(transaction, "proxy_selection_changed", operationID, now)
		return err
	})
}

func validProxySelectionName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 256 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
