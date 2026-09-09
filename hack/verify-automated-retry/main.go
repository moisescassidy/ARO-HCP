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

// verify-automated-retry walks pipeline YAML files under the given directories
// and checks every automatedRetry block against the limits EV2 enforces on the
// generated rollout manifest, plus the wall-clock delay budget those blocks
// commit a rollout to.
//
// EV2 silently drops or rejects a retry policy that exceeds its limits, and a
// dropped policy is indistinguishable from no policy at all until a transient
// failure hard-fails a Stage rollout. The limits are documented on the
// AutomatedRetry type in github.com/Azure/ARO-Tools/pipelines/types:
//
//   - errorContainsAny: 16 or fewer items, 1KB or less encoded
//   - maximumRetryCount: between 1 and 10
//   - durationBetweenRetries: between 1 minute and 3 hours
//
// Exit code is 1 if any violations are found.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	dirs := os.Args[1:]
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: verify-automated-retry <dir> [<dir> ...]")
		os.Exit(2)
	}

	var blocks []RetryBlock
	for _, dir := range dirs {
		b, err := CollectRetryBlocks(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error processing %s: %v\n", dir, err)
			os.Exit(2)
		}
		blocks = append(blocks, b...)
	}

	var violations []string
	for _, b := range blocks {
		violations = append(violations, b.Check()...)
	}

	if len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "ERROR: The following automatedRetry blocks are invalid.")
		fmt.Fprintln(os.Stderr, "       EV2 drops or rejects a policy that exceeds its limits, and a dropped")
		fmt.Fprintln(os.Stderr, "       policy looks exactly like no policy until a transient failure fails a")
		fmt.Fprintln(os.Stderr, "       Stage rollout.")
		fmt.Fprintln(os.Stderr)
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "  "+v)
		}
		os.Exit(1)
	}

	fmt.Printf("verify-automated-retry: %d automatedRetry blocks OK\n", len(blocks))
}

// pipelineFile is the subset of the pipeline schema this tool cares about.
// It is deliberately not the full ARO-Tools type: those fields are templated
// and need config resolution to load, while errorContainsAny entries are
// always literal strings.
type pipelineFile struct {
	ServiceGroup   string `yaml:"serviceGroup"`
	ResourceGroups []struct {
		Name  string `yaml:"name"`
		Steps []struct {
			Name           string          `yaml:"name"`
			AutomatedRetry *AutomatedRetry `yaml:"automatedRetry"`
		} `yaml:"steps"`
	} `yaml:"resourceGroups"`
}

// AutomatedRetry mirrors the ARO-Tools pipeline type.
type AutomatedRetry struct {
	ErrorContainsAny       []string `yaml:"errorContainsAny"`
	MaximumRetryCount      int      `yaml:"maximumRetryCount"`
	DurationBetweenRetries string   `yaml:"durationBetweenRetries"`
}

// RetryBlock is one automatedRetry block located in the tree.
type RetryBlock struct {
	File          string
	ServiceGroup  string
	ResourceGroup string
	Step          string
	Retry         AutomatedRetry
}

// String identifies the block in violation messages.
func (b RetryBlock) String() string {
	return fmt.Sprintf("%s: %s/%s", b.File, b.ResourceGroup, b.Step)
}

// isPipelineFile reports whether a path looks like a rollout pipeline
// definition. Pipelines are named either "pipeline.yaml" or "<x>-pipeline.yaml".
func isPipelineFile(path string) bool {
	base := filepath.Base(path)
	return base == "pipeline.yaml" || strings.HasSuffix(base, "-pipeline.yaml")
}

// CollectRetryBlocks walks dir and returns every automatedRetry block found in
// a pipeline file.
func CollectRetryBlocks(dir string) ([]RetryBlock, error) {
	var blocks []RetryBlock
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip trees whose pipeline files are not real rollout definitions:
			// vendored copies we do not own, and testdata fixtures that hold
			// deliberately malformed or partially templated YAML.
			switch info.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !isPipelineFile(path) {
			return nil
		}
		found, err := parseFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		blocks = append(blocks, found...)
		return nil
	})
	return blocks, err
}
