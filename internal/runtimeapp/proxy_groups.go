package runtimeapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"submux/internal/mihomo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimestate"
)

type ProxyControl interface {
	ProxyGroups(context.Context) (map[string]runtimeprocess.ControlProxy, error)
	SelectProxy(context.Context, string, string) error
	ProxyDelay(context.Context, string) (int, error)
}

func (e *MihomoExecutor) proxyControl() ProxyControl {
	if e.ProxyControl != nil {
		return e.ProxyControl
	}
	return runtimeprocess.ControlProbe{Endpoint: e.ControlEndpoint}
}

func (e *MihomoExecutor) ProxyGroups(ctx context.Context, query runtimeapi.ProxyGroupQuery) (runtimeapi.ProxyGroupList, error) {
	if e == nil || e.State == nil || e.Process == nil {
		return runtimeapi.ProxyGroupList{}, errors.New("Runtime proxy group viewer is unavailable")
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return runtimeapi.ProxyGroupList{}, err
	}
	record, current, err := e.proxyGroupSource(query.SourceID)
	if err != nil {
		return runtimeapi.ProxyGroupList{}, err
	}
	_, candidate, err := e.State.ReadSourceRevision(record)
	if err != nil {
		return runtimeapi.ProxyGroupList{}, err
	}
	groups, err := mihomo.CandidateProxyGroups(candidate)
	if err != nil {
		return runtimeapi.ProxyGroupList{}, err
	}
	selections, err := e.State.ProxySelections(record.ID)
	if err != nil {
		return runtimeapi.ProxyGroupList{}, err
	}
	selectionByGroup := make(map[string]string, len(selections))
	for _, selection := range selections {
		selectionByGroup[selection.Group] = selection.Node
	}
	result := runtimeapi.ProxyGroupList{
		SourceID:      record.ID,
		CurrentSource: current,
		Available:     current,
		ObservedAt:    e.now(),
		Groups:        make([]runtimeapi.ProxyGroupStatus, 0, len(groups)),
	}
	for _, group := range groups {
		status := runtimeapi.ProxyGroupStatus{
			Name: group.Name, Type: group.Type, Main: group.Main, Selectable: group.Selectable,
			Current: group.Current, Nodes: make([]runtimeapi.ProxyNodeStatus, 0, len(group.Nodes)),
		}
		if selected := selectionByGroup[group.Name]; selected != "" && candidateNode(group, selected) != nil {
			status.Current = selected
		}
		for _, node := range group.Nodes {
			status.Nodes = append(status.Nodes, runtimeapi.ProxyNodeStatus{
				Name: node.Name, Type: node.Type, Available: node.Available,
				UnavailableReason: node.UnavailableReason,
			})
		}
		result.Groups = append(result.Groups, status)
	}
	delays, err := e.State.ProxyDelayResults(record.ID)
	if err != nil {
		return runtimeapi.ProxyGroupList{}, err
	}
	overlayPersistedProxyDelays(&result, delays, e.now())
	if !current {
		result.Available = false
		result.Message = "切换为当前来源后可以选择节点"
		return result, nil
	}
	running, err := e.Process.IsRunning(ctx)
	if err != nil {
		return runtimeapi.ProxyGroupList{}, err
	}
	if !running {
		result.Available = false
		result.Message = "Mihomo 未运行；选择会在下次启动时生效"
		return result, nil
	}
	live, err := e.proxyControl().ProxyGroups(ctx)
	if err != nil {
		result.Available = false
		result.Message = "暂时无法读取 Mihomo 代理组状态"
		return result, nil
	}
	result.Available = true
	overlayLiveProxyGroups(&result, live, e.now())
	overlayPersistedProxyDelays(&result, delays, e.now())
	return result, nil
}

func (e *MihomoExecutor) proxyGroupSource(sourceID string) (runtimestate.SourceRecord, bool, error) {
	current, currentErr := e.State.CurrentSource()
	if sourceID == "" {
		if currentErr != nil {
			return runtimestate.SourceRecord{}, false, currentErr
		}
		return current, true, nil
	}
	record, err := e.State.GetSource(sourceID)
	if err != nil {
		return runtimestate.SourceRecord{}, false, err
	}
	return record, currentErr == nil && current.ID == record.ID, nil
}

func overlayLiveProxyGroups(result *runtimeapi.ProxyGroupList, live map[string]runtimeprocess.ControlProxy, now time.Time) {
	for groupIndex := range result.Groups {
		group := &result.Groups[groupIndex]
		liveGroup, exists := live[group.Name]
		if !exists {
			continue
		}
		if liveGroup.Now != "" {
			group.Current = liveGroup.Now
		}
		for _, liveName := range liveGroup.All {
			if runtimeProxyNode(group.Nodes, liveName) != nil {
				continue
			}
			liveNode := live[liveName]
			group.Nodes = append(group.Nodes, runtimeapi.ProxyNodeStatus{
				Name: liveName, Type: liveNode.Type, Available: liveNode.Alive == nil || *liveNode.Alive,
			})
		}
		for nodeIndex := range group.Nodes {
			node := &group.Nodes[nodeIndex]
			liveNode, exists := live[node.Name]
			if !exists {
				continue
			}
			if liveNode.Type != "" {
				node.Type = liveNode.Type
			}
			if liveNode.Alive != nil && !*liveNode.Alive {
				node.Available = false
				node.UnavailableReason = "Mihomo 最近一次探测失败"
			}
			if length := len(liveNode.History); length > 0 {
				last := liveNode.History[length-1]
				testedAt := last.Time.UTC()
				if node.DelayTestedAt == nil || testedAt.After(*node.DelayTestedAt) {
					node.DelayMillis = last.Delay
					node.DelayTestedAt = &testedAt
					node.DelayFailure = ""
					node.DelayStale = now.Sub(testedAt) > 5*time.Minute
				}
			}
		}
	}
}

func overlayPersistedProxyDelays(result *runtimeapi.ProxyGroupList, records []runtimestate.ProxyDelayRecord, now time.Time) {
	for _, record := range records {
		for groupIndex := range result.Groups {
			group := &result.Groups[groupIndex]
			if group.Name != record.Group {
				continue
			}
			node := runtimeProxyNode(group.Nodes, record.Node)
			if node == nil {
				break
			}
			testedAt := record.TestedAt.UTC()
			if node.DelayTestedAt != nil && !testedAt.After(*node.DelayTestedAt) {
				break
			}
			node.DelayMillis = record.DelayMillis
			node.DelayTestedAt = &testedAt
			node.DelayFailure = record.Failure
			node.DelayStale = now.Sub(testedAt) > 5*time.Minute
			break
		}
	}
}

func (e *MihomoExecutor) selectProxyNode(ctx context.Context, operation runtimeapi.Operation, report StageReporter) (*runtimeapi.OperationResult, error) {
	params := operation.Action.Params
	if err := report("validating_proxy_selection", 15, true); err != nil {
		return nil, err
	}
	current, err := e.State.CurrentSource()
	if err != nil || current.ID != params.SourceID {
		return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "只能修改当前来源的代理组选择", Cause: err}
	}
	_, candidate, err := e.State.ReadSourceRevision(current)
	if err != nil {
		return nil, err
	}
	groups, err := mihomo.CandidateProxyGroups(candidate)
	if err != nil {
		return nil, err
	}
	group := candidateGroup(groups, params.ProxyGroup)
	if group == nil || !group.Selectable {
		return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "候选配置中不存在可选择的代理组"}
	}
	node := candidateNode(*group, params.ProxyNode)
	if node != nil && !node.Available {
		return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "候选配置中不存在可用的目标节点"}
	}
	running, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	if !running && node == nil {
		return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "Mihomo 未运行，无法验证代理提供者中的目标节点"}
	}
	previous := group.Current
	if selections, selectionErr := e.State.ProxySelections(current.ID); selectionErr == nil {
		for _, selection := range selections {
			if selection.Group == group.Name {
				previous = selection.Node
				break
			}
		}
	}
	if running {
		live, liveErr := e.proxyControl().ProxyGroups(ctx)
		if liveErr != nil {
			return nil, &PublicError{Code: runtimeapi.ErrorServiceUnavailable, Message: "无法读取 Mihomo 当前代理组", Retryable: true, Cause: liveErr}
		}
		liveGroup, exists := live[group.Name]
		if !exists || !containsName(liveGroup.All, params.ProxyNode) {
			return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "Mihomo 当前配置中不存在目标节点"}
		}
		if liveGroup.Now != "" {
			previous = liveGroup.Now
		}
		if err := report("selecting_proxy_node", 65, false); err != nil {
			return nil, err
		}
		if err := e.proxyControl().SelectProxy(ctx, group.Name, params.ProxyNode); err != nil {
			return nil, &PublicError{Code: runtimeapi.ErrorServiceUnavailable, Message: "Mihomo 拒绝了代理节点选择", Retryable: true, Cause: err}
		}
	}
	if err := report("saving_proxy_selection", 85, false); err != nil {
		return nil, err
	}
	if err := e.State.SetProxySelection(current.ID, group.Name, params.ProxyNode, operation.ID, e.now()); err != nil {
		if running && previous != "" && previous != params.ProxyNode {
			_ = e.proxyControl().SelectProxy(context.Background(), group.Name, previous)
		}
		return nil, err
	}
	return &runtimeapi.OperationResult{SourceID: current.ID, ProxyGroup: group.Name, ProxyNode: params.ProxyNode}, nil
}

func (e *MihomoExecutor) replayProxySelections(ctx context.Context, sourceID string) error {
	selections, err := e.State.ProxySelections(sourceID)
	if err != nil {
		return err
	}
	if len(selections) == 0 {
		return nil
	}
	record, err := e.State.GetSource(sourceID)
	if err != nil {
		return err
	}
	_, candidate, err := e.State.ReadSourceRevision(record)
	if err != nil {
		return err
	}
	groups, err := mihomo.CandidateProxyGroups(candidate)
	if err != nil {
		return err
	}
	var live map[string]runtimeprocess.ControlProxy
	for _, selection := range selections {
		group := candidateGroup(groups, selection.Group)
		if group == nil || !group.Selectable {
			continue
		}
		node := candidateNode(*group, selection.Node)
		if node != nil && !node.Available {
			continue
		}
		if node == nil {
			if len(group.Providers) == 0 {
				continue
			}
			if live == nil {
				live, err = e.proxyControl().ProxyGroups(ctx)
				if err != nil {
					return err
				}
			}
			liveGroup, exists := live[group.Name]
			if !exists || !containsName(liveGroup.All, selection.Node) {
				continue
			}
		}
		if err := e.proxyControl().SelectProxy(ctx, group.Name, selection.Node); err != nil {
			return fmt.Errorf("restore proxy selection %s: %w", group.Name, err)
		}
	}
	return nil
}

func runtimeProxyNode(nodes []runtimeapi.ProxyNodeStatus, name string) *runtimeapi.ProxyNodeStatus {
	for index := range nodes {
		if nodes[index].Name == name {
			return &nodes[index]
		}
	}
	return nil
}

func candidateGroup(groups []mihomo.CandidateProxyGroup, name string) *mihomo.CandidateProxyGroup {
	for index := range groups {
		if groups[index].Name == name {
			return &groups[index]
		}
	}
	return nil
}

func candidateNode(group mihomo.CandidateProxyGroup, name string) *mihomo.CandidateProxyNode {
	for index := range group.Nodes {
		if group.Nodes[index].Name == name {
			return &group.Nodes[index]
		}
	}
	return nil
}

func containsName(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func validProxyActionName(value string) bool {
	if strings.TrimSpace(value) != value || value == "" || len([]rune(value)) > runtimeapi.ProxyGroupNameMaxLength {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validProxyLatencyParams(params runtimeapi.ActionParams) bool {
	if !validSourceID(params.SourceID) {
		return false
	}
	switch params.LatencyScope {
	case runtimeapi.ProxyLatencyScopeNode:
		return validProxyActionName(params.ProxyGroup) && validProxyActionName(params.ProxyNode)
	case runtimeapi.ProxyLatencyScopeGroup:
		return validProxyActionName(params.ProxyGroup) && params.ProxyNode == ""
	case runtimeapi.ProxyLatencyScopeSource:
		return params.ProxyGroup == "" && params.ProxyNode == ""
	default:
		return false
	}
}
