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
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Limits EV2 enforces on an automatedRetry block. See the AutomatedRetry type
// in github.com/Azure/ARO-Tools/pipelines/types.
const (
	MaxErrorContainsAnyItems  = 16
	MaxErrorContainsAnyBytes  = 1024
	MinRetryCount             = 1
	MaxRetryCount             = 10
	MinDurationBetweenRetries = time.Minute
	MaxDurationBetweenRetries = 3 * time.Hour
)

// MaxWorstCaseDelay is the wall-clock delay a single step may commit a rollout
// to before we consider it a problem for on-call.
//
// This is our budget, not an EV2 limit. When a retry fires, the rollout waits
// DurationBetweenRetries before each of MaximumRetryCount attempts, and that
// waiting is pure latency added to the Stage. EV2's own limits permit
// 10 x 3h = 30 hours, which no ARO-HCP step should ever ask for.
const MaxWorstCaseDelay = 30 * time.Minute

func parseFile(path string) ([]RetryBlock, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p pipelineFile
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, err
	}

	var blocks []RetryBlock
	for _, rg := range p.ResourceGroups {
		for _, step := range rg.Steps {
			if step.AutomatedRetry == nil {
				continue
			}
			blocks = append(blocks, RetryBlock{
				File:          path,
				ServiceGroup:  p.ServiceGroup,
				ResourceGroup: rg.Name,
				Step:          step.Name,
				Retry:         *step.AutomatedRetry,
			})
		}
	}
	return blocks, nil
}

// Matches reports whether errText would trigger this retry policy, using EV2's
// documented semantics: a retry fires if the step output contains any entry in
// errorContainsAny, compared case-insensitively.
func (r AutomatedRetry) Matches(errText string) bool {
	return r.MatchingEntry(errText) != ""
}

// MatchingEntry returns the first errorContainsAny entry that matches errText,
// or "" if none do. Callers use this to report *which* entry fired.
func (r AutomatedRetry) MatchingEntry(errText string) string {
	lowered := strings.ToLower(errText)
	for _, entry := range r.ErrorContainsAny {
		if strings.Contains(lowered, strings.ToLower(entry)) {
			return entry
		}
	}
	return ""
}

// EncodedSize is the size of errorContainsAny as EV2 counts it against the 1KB
// cap, i.e. the encoded array rather than the sum of the raw strings.
func (r AutomatedRetry) EncodedSize() int {
	encoded, err := json.Marshal(r.ErrorContainsAny)
	if err != nil {
		// A []string cannot fail to marshal; fall back to a conservative
		// over-estimate rather than swallowing the error.
		total := 2
		for _, e := range r.ErrorContainsAny {
			total += len(e) + 3
		}
		return total
	}
	return len(encoded)
}

// WorstCaseDelay is the wall-clock time this policy can add to a rollout, not
// counting the time each re-attempt of the step itself takes.
func (r AutomatedRetry) WorstCaseDelay() (time.Duration, error) {
	if r.DurationBetweenRetries == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(r.DurationBetweenRetries)
	if err != nil {
		return 0, fmt.Errorf("durationBetweenRetries %q is not a valid Go duration: %w", r.DurationBetweenRetries, err)
	}
	count := r.MaximumRetryCount
	if count == 0 {
		count = 1 // EV2 default
	}
	return d * time.Duration(count), nil
}

// RedundantEntries returns entries that are subsumed by another entry in the
// same list, i.e. that can never be the reason a retry fires. EV2 matches
// case-insensitively, so "InternalServerError" makes "internalservererror"
// dead weight against the 1KB cap.
func (r AutomatedRetry) RedundantEntries() []string {
	var redundant []string
	for i, a := range r.ErrorContainsAny {
		for j, b := range r.ErrorContainsAny {
			if i == j {
				continue
			}
			// b subsumes a when a contains b; on an exact case-insensitive tie
			// keep the first occurrence so we report each pair once.
			la, lb := strings.ToLower(a), strings.ToLower(b)
			if la == lb {
				if j < i {
					redundant = append(redundant, a)
					break
				}
				continue
			}
			if strings.Contains(la, lb) {
				redundant = append(redundant, a)
				break
			}
		}
	}
	return redundant
}

// Check returns a violation message for each limit this block breaks.
func (b RetryBlock) Check() []string {
	var violations []string
	add := func(format string, args ...any) {
		violations = append(violations, fmt.Sprintf("%s: %s", b, fmt.Sprintf(format, args...)))
	}
	r := b.Retry

	if len(r.ErrorContainsAny) == 0 {
		add("errorContainsAny is empty, so the retry can never fire")
	}
	if n := len(r.ErrorContainsAny); n > MaxErrorContainsAnyItems {
		add("errorContainsAny has %d items, EV2 allows at most %d", n, MaxErrorContainsAnyItems)
	}
	if size := r.EncodedSize(); size > MaxErrorContainsAnyBytes {
		add("errorContainsAny encodes to %d bytes, EV2 allows at most %d", size, MaxErrorContainsAnyBytes)
	}
	for _, entry := range r.RedundantEntries() {
		add("errorContainsAny entry %q is already covered by another entry (EV2 matches case-insensitively)", entry)
	}
	if r.MaximumRetryCount != 0 && (r.MaximumRetryCount < MinRetryCount || r.MaximumRetryCount > MaxRetryCount) {
		add("maximumRetryCount is %d, EV2 requires between %d and %d", r.MaximumRetryCount, MinRetryCount, MaxRetryCount)
	}

	delay, err := r.WorstCaseDelay()
	if err != nil {
		add("%v", err)
		return violations
	}
	if r.DurationBetweenRetries != "" {
		d, _ := time.ParseDuration(r.DurationBetweenRetries)
		if d < MinDurationBetweenRetries || d > MaxDurationBetweenRetries {
			add("durationBetweenRetries is %s, EV2 requires between %s and %s",
				d, MinDurationBetweenRetries, MaxDurationBetweenRetries)
		}
	}
	if delay > MaxWorstCaseDelay {
		add("worst case adds %s of waiting to the rollout, budget is %s", delay, MaxWorstCaseDelay)
	}

	return violations
}
