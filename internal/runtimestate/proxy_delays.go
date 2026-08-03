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

var proxyDelaysBucket = []byte("runtime_proxy_delays")

type ProxyDelayRecord struct {
	SourceID    string    `json:"source_id"`
	Group       string    `json:"group"`
	Node        string    `json:"node"`
	DelayMillis int       `json:"delay_millis"`
	TestedAt    time.Time `json:"tested_at"`
	Failure     string    `json:"failure,omitempty"`
}

func (s *Store) ProxyDelayResults(sourceID string) ([]ProxyDelayRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("Runtime state is not open")
	}
	if !validSourceID(sourceID) {
		return nil, errors.New("Runtime source ID is invalid")
	}
	prefix := []byte(sourceID + "\x00")
	result := make([]ProxyDelayRecord, 0)
	err := s.db.View(func(transaction *bbolt.Tx) error {
		if transaction.Bucket(sourcesBucket).Get([]byte(sourceID)) == nil {
			return ErrSourceNotFound
		}
		bucket := transaction.Bucket(proxyDelaysBucket)
		cursor := bucket.Cursor()
		for key, value := cursor.Seek(prefix); key != nil && strings.HasPrefix(string(key), string(prefix)); key, value = cursor.Next() {
			var record ProxyDelayRecord
			if err := json.Unmarshal(value, &record); err != nil || !validProxyDelayRecord(record) || record.SourceID != sourceID {
				return errors.New("Runtime proxy delay record is invalid")
			}
			result = append(result, record)
		}
		return nil
	})
	sort.Slice(result, func(i, j int) bool {
		if result[i].Group == result[j].Group {
			return result[i].Node < result[j].Node
		}
		return result[i].Group < result[j].Group
	})
	return result, err
}

func (s *Store) SetProxyDelayResult(
	sourceID string,
	group string,
	node string,
	delayMillis int,
	failure string,
	operationID string,
	now time.Time,
) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	record := ProxyDelayRecord{
		SourceID: sourceID, Group: group, Node: node, DelayMillis: delayMillis,
		TestedAt: now.UTC(), Failure: failure,
	}
	if !validProxyDelayRecord(record) {
		return errors.New("Runtime proxy delay result is invalid")
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.db.Update(func(transaction *bbolt.Tx) error {
		if transaction.Bucket(sourcesBucket).Get([]byte(sourceID)) == nil {
			return ErrSourceNotFound
		}
		key := []byte(sourceID + "\x00" + group + "\x00" + node)
		if err := transaction.Bucket(proxyDelaysBucket).Put(key, body); err != nil {
			return err
		}
		_, err := advanceRevisionAndEvent(transaction, "proxy_delay.updated", operationID, now)
		return err
	})
}

func validProxyDelayRecord(record ProxyDelayRecord) bool {
	if !validSourceID(record.SourceID) || !validProxySelectionName(record.Group) ||
		!validProxySelectionName(record.Node) || record.DelayMillis < 0 || record.TestedAt.IsZero() {
		return false
	}
	if record.Failure != "" && record.DelayMillis != 0 {
		return false
	}
	if utf8.RuneCountInString(record.Failure) > 512 {
		return false
	}
	for _, character := range record.Failure {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
