package runtimeapp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"submux/internal/mihomo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimeprocess"
)

const proxyLatencyConcurrency = 4

type proxyLatencyTarget struct {
	Node   string
	Groups []string
}

type proxyLatencyResult struct {
	Target proxyLatencyTarget
	Delay  int
	Err    error
}

func (e *MihomoExecutor) testProxyLatency(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	if err := report("validating_proxy_latency", 10, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params := operation.Action.Params
	current, err := e.State.CurrentSource()
	if err != nil || current.ID != params.SourceID {
		return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "只能测试当前来源的代理节点", Cause: err}
	}
	running, err := e.Process.IsRunning(ctx)
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, &PublicError{Code: runtimeapi.ErrorServiceUnavailable, Message: "Mihomo 未运行，无法测试代理延迟", Retryable: true}
	}
	_, candidate, err := e.State.ReadSourceRevision(current)
	if err != nil {
		return nil, err
	}
	groups, err := mihomo.CandidateProxyGroups(candidate)
	if err != nil {
		return nil, err
	}
	live, err := e.proxyControl().ProxyGroups(ctx)
	if err != nil {
		return nil, &PublicError{Code: runtimeapi.ErrorServiceUnavailable, Message: "无法读取 Mihomo 当前代理组", Retryable: true, Cause: err}
	}
	targets, err := proxyLatencyTargets(params, groups, live)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "当前测试范围没有可测试的代理节点"}
	}
	if err := report("testing_proxy_latency", 15, true); err != nil {
		return nil, err
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan proxyLatencyTarget)
	results := make(chan proxyLatencyResult, len(targets))
	workers := proxyLatencyConcurrency
	if len(targets) < workers {
		workers = len(targets)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for target := range jobs {
				delay, probeErr := e.proxyControl().ProxyDelay(runContext, target.Node)
				select {
				case results <- proxyLatencyResult{Target: target, Delay: delay, Err: probeErr}:
				case <-runContext.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, target := range targets {
			select {
			case jobs <- target:
			case <-runContext.Done():
				return
			}
		}
	}()
	go func() {
		wait.Wait()
		close(results)
	}()

	result := &runtimeapi.OperationResult{SourceID: current.ID}
	for probe := range results {
		if err := ctx.Err(); err != nil {
			continue
		}
		failure := proxyLatencyFailure(probe.Err)
		if failure == "" {
			result.LatencySucceeded++
		} else {
			probe.Delay = 0
			result.LatencyFailed++
		}
		for _, group := range probe.Target.Groups {
			if err := e.State.SetProxyDelayResult(
				current.ID, group, probe.Target.Node, probe.Delay, failure, operation.ID, e.now(),
			); err != nil {
				cancel()
				return nil, err
			}
		}
		result.LatencyTested++
		progress := 15 + result.LatencyTested*80/len(targets)
		if err := report("testing_proxy_latency", progress, true); err != nil {
			cancel()
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func proxyLatencyTargets(
	params runtimeapi.ActionParams,
	groups []mihomo.CandidateProxyGroup,
	live map[string]runtimeprocess.ControlProxy,
) ([]proxyLatencyTarget, error) {
	selectedGroups := groups
	if params.LatencyScope == runtimeapi.ProxyLatencyScopeNode || params.LatencyScope == runtimeapi.ProxyLatencyScopeGroup {
		group := candidateGroup(groups, params.ProxyGroup)
		if group == nil {
			return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "当前来源中不存在目标代理组"}
		}
		selectedGroups = []mihomo.CandidateProxyGroup{*group}
	}
	byNode := make(map[string]*proxyLatencyTarget)
	order := make([]string, 0)
	for _, group := range selectedGroups {
		liveGroup, exists := live[group.Name]
		if !exists {
			if params.LatencyScope != runtimeapi.ProxyLatencyScopeSource {
				return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "Mihomo 当前配置中不存在目标代理组"}
			}
			continue
		}
		for _, node := range liveGroup.All {
			if params.LatencyScope == runtimeapi.ProxyLatencyScopeNode && node != params.ProxyNode {
				continue
			}
			target := byNode[node]
			if target == nil {
				target = &proxyLatencyTarget{Node: node}
				byNode[node] = target
				order = append(order, node)
			}
			target.Groups = append(target.Groups, group.Name)
		}
	}
	if params.LatencyScope == runtimeapi.ProxyLatencyScopeNode && byNode[params.ProxyNode] == nil {
		return nil, &PublicError{Code: runtimeapi.ErrorInvalidRequest, Message: "Mihomo 当前代理组中不存在目标节点"}
	}
	targets := make([]proxyLatencyTarget, 0, len(order))
	for _, node := range order {
		targets = append(targets, *byNode[node])
	}
	return targets, nil
}

func proxyLatencyFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "测试超时"
	}
	if errors.Is(err, context.Canceled) {
		return "测试已取消"
	}
	return fmt.Sprintf("测试失败：%s", publicControlError(err))
}

func publicControlError(err error) string {
	if err == nil {
		return "未知错误"
	}
	var public *PublicError
	if errors.As(err, &public) && public.Message != "" {
		return public.Message
	}
	return "Mihomo 未返回有效结果"
}
