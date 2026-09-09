// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"testing"
	"time"
)

// repoRoot is where the pipeline files live relative to this module.
const repoRoot = "../.."

// clusterStepPipelines are the steps that create or update an AKS cluster.
// Both share dev-infrastructure/modules/aks-cluster-base.bicep, which sets
// azureMonitorProfile.metrics.enabled, so both make AKS reconcile the managed
// Prometheus core extension on every managed-cluster write.
var clusterStepPipelines = []struct {
	file          string
	resourceGroup string
	step          string
}{
	{"dev-infrastructure/svc-pipeline.yaml", "service", "cluster"},
	{"dev-infrastructure/mgmt-pipeline.yaml", "management", "cluster"},
}

// findBlock locates a single automatedRetry block by resource group and step.
func findBlock(t *testing.T, file, resourceGroup, step string) RetryBlock {
	t.Helper()
	blocks, err := parseFile(repoRoot + "/" + file)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	for _, b := range blocks {
		if b.ResourceGroup == resourceGroup && b.Step == step {
			return b
		}
	}
	t.Fatalf("no automatedRetry block for %s/%s in %s", resourceGroup, step, file)
	return RetryBlock{}
}

// TestClusterStepRetriesAksExtensionConflict is the AROSLSRE-1864 regression
// test. The Stage rollout for Service.Infra.service.cluster-uksouth-1 hard
// failed on this exact error, which Microsoft's cluster extension TSG
// documents as a lock to wait out and retry rather than a bad chart. The step
// already declared automatedRetry, but nothing in errorContainsAny matched, so
// a documented-transient failure consumed a Stage rollout.
func TestClusterStepRetriesAksExtensionConflict(t *testing.T) {
	t.Parallel()

	// Verbatim from the failed rollout, including the surrounding ARM envelope,
	// so the test exercises the matcher against what EV2 actually sees rather
	// than against a hand-tidied substring.
	const observedError = `Code: DeploymentFailed
Target: /subscriptions/94299c3d-4a34-4440-98ac-de3d66ee2338/resourceGroups/hcp-underlay-stg-uksouth-svc/providers/Microsoft.Resources/deployments/cluster-sby62i2tkescs
  InnerError: ResourceDeploymentFailure
  Target: /subscriptions/94299c3d-4a34-4440-98ac-de3d66ee2338/resourceGroups/hcp-underlay-stg-uksouth-svc/providers/Microsoft.ContainerService/managedClusters/stg-uksouth-svc-1
    InnerError: CreateOrUpdateExtensionFailed
    Create or update of core cluster extension aks-managed-azure-monitor-metrics
    of type microsoft.azuremonitor.containers.metrics failed.
    Update failed for this resource, as there is a conflicting operation in progress.
    Please try after sometime.`

	for _, p := range clusterStepPipelines {
		t.Run(p.file, func(t *testing.T) {
			t.Parallel()
			b := findBlock(t, p.file, p.resourceGroup, p.step)
			entry := b.Retry.MatchingEntry(observedError)
			if entry == "" {
				t.Errorf("the AROSLSRE-1864 rollout failure would not retry.\n"+
					"%s declares automatedRetry but no errorContainsAny entry matches:\n%s\n"+
					"configured entries: %q",
					b, observedError, b.Retry.ErrorContainsAny)
			} else {
				t.Logf("%s retries on %q", b, entry)
			}
		})
	}
}

// TestAksExtensionConflictMatchIsNotOverBroad guards the deliberate decision to
// match on the message rather than on the CreateOrUpdateExtensionFailed
// inner-error code. That code covers all extension write failures, including
// non-transient ones: AROSLSRE-1632 was a policy denial on aks-managed-dranet,
// where retrying only burns the retry budget before failing anyway.
func TestAksExtensionConflictMatchIsNotOverBroad(t *testing.T) {
	t.Parallel()

	// AROSLSRE-1632 shape: same inner-error code, not a lock, not retryable.
	const policyDenial = `InnerError: CreateOrUpdateExtensionFailed
Create or update of core cluster extension aks-managed-dranet
of type microsoft.dranet failed.
Reason: admission webhook denied the request, blocked by policy.`

	for _, p := range clusterStepPipelines {
		t.Run(p.file, func(t *testing.T) {
			t.Parallel()
			b := findBlock(t, p.file, p.resourceGroup, p.step)
			if entry := b.Retry.MatchingEntry(policyDenial); entry != "" {
				t.Errorf("%s would retry a non-transient extension failure because of entry %q.\n"+
					"Retrying a policy denial cannot succeed and delays the real failure by the "+
					"full retry budget.", b, entry)
			}
		})
	}
}

// TestMatchesUsesCaseInsensitiveContains pins the matcher to EV2's documented
// semantics. If this drifts, every other test here is measuring the wrong thing.
func TestMatchesUsesCaseInsensitiveContains(t *testing.T) {
	t.Parallel()

	r := AutomatedRetry{ErrorContainsAny: []string{"conflicting operation in progress"}}

	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"exact", "conflicting operation in progress", true},
		{"embedded in ARM envelope", "...as there is a conflicting operation in progress. Please try...", true},
		{"different case", "CONFLICTING OPERATION IN PROGRESS", true},
		{"unrelated error", "Insufficient regional vcpu quota remaining", false},
		{"empty output", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := r.Matches(tc.text); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestAllRetryBlocksRespectEv2Limits sweeps every pipeline in the repo. EV2
// drops or rejects a policy that breaks these limits, and a dropped policy is
// indistinguishable from no policy until a transient failure fails a rollout,
// so exceeding a limit is silent until it is expensive.
func TestAllRetryBlocksRespectEv2Limits(t *testing.T) {
	t.Parallel()

	blocks, err := CollectRetryBlocks(repoRoot)
	if err != nil {
		t.Fatalf("collecting retry blocks: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("found no automatedRetry blocks; the walk is probably broken")
	}
	t.Logf("checked %d automatedRetry blocks", len(blocks))

	for _, b := range blocks {
		for _, v := range b.Check() {
			t.Error(v)
		}
	}
}

// TestRetryDelayBudget is the performance test.
//
// The cost of this change is not CPU, it is wall clock. When a retry fires the
// rollout sits idle for durationBetweenRetries before each attempt, and adding
// an errorContainsAny entry widens the set of failures that pay that cost.
// EV2's own limits permit 10 x 3h = 30 hours of waiting per step, which would
// silently turn a Stage rollout into a multi-day event, so the repo needs its
// own budget.
func TestRetryDelayBudget(t *testing.T) {
	t.Parallel()

	blocks, err := CollectRetryBlocks(repoRoot)
	if err != nil {
		t.Fatalf("collecting retry blocks: %v", err)
	}

	var worst RetryBlock
	var worstDelay time.Duration
	for _, b := range blocks {
		delay, err := b.Retry.WorstCaseDelay()
		if err != nil {
			t.Errorf("%s: %v", b, err)
			continue
		}
		if delay > MaxWorstCaseDelay {
			t.Errorf("%s: worst case adds %s of waiting, budget is %s", b, delay, MaxWorstCaseDelay)
		}
		if delay > worstDelay {
			worst, worstDelay = b, delay
		}
	}
	t.Logf("slowest step is %s at %s worst case (budget %s)", worst, worstDelay, MaxWorstCaseDelay)
}

// TestClusterStepDelayCoversTheTsgWait documents the gap this change does not
// close. Microsoft's TSG says to wait ~10 minutes before retrying an
// "operation is already in progress" conflict. The cluster step waits
// 3 x 2m = 6m, so a collision with a slow AKS-initiated node image upgrade can
// still exhaust the budget and hard-fail.
//
// This is skipped rather than failing: durationBetweenRetries is per step and
// shared with every other error class in the same list, so lengthening it
// slows unrelated transients. Raising maximumRetryCount instead makes a
// genuinely broken AKS deploy retry five times. Both are reviewer calls, and
// the test records the tradeoff so it is not silently forgotten.
func TestClusterStepDelayCoversTheTsgWait(t *testing.T) {
	t.Parallel()

	// https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/extensions/cluster-extension-deployment-errors
	const tsgRecommendedWait = 10 * time.Minute

	for _, p := range clusterStepPipelines {
		t.Run(p.file, func(t *testing.T) {
			t.Parallel()
			b := findBlock(t, p.file, p.resourceGroup, p.step)
			delay, err := b.Retry.WorstCaseDelay()
			if err != nil {
				t.Fatalf("%s: %v", b, err)
			}
			if delay < tsgRecommendedWait {
				t.Skipf("known gap (AROSLSRE-1864): %s waits at most %s (%d x %s), "+
					"Microsoft's TSG recommends ~%s before retrying an extension conflict",
					b, delay, b.Retry.MaximumRetryCount, b.Retry.DurationBetweenRetries, tsgRecommendedWait)
			}
		})
	}
}

// BenchmarkMatches measures the matcher itself.
//
// This number is not decision relevant and no threshold is asserted on it.
// errorContainsAny is capped at 16 short strings and is evaluated once per
// failed step, against a retry that then waits minutes. The benchmark exists
// to make that explicit: if a future change makes matching expensive enough to
// notice here, it has gone badly wrong somewhere else.
func BenchmarkMatches(b *testing.B) {
	blocks, err := CollectRetryBlocks(repoRoot)
	if err != nil {
		b.Fatalf("collecting retry blocks: %v", err)
	}
	var retry AutomatedRetry
	for _, blk := range blocks {
		if blk.ResourceGroup == "service" && blk.Step == "cluster" {
			retry = blk.Retry
			break
		}
	}
	// Worst case for the matcher: no entry matches, so every one is scanned.
	const noMatch = "Code: DeploymentFailed\nInnerError: SomethingEntirelyUnrelated"

	b.ReportAllocs()
	for b.Loop() {
		if retry.Matches(noMatch) {
			b.Fatal("expected no match")
		}
	}
}
