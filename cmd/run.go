// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/c4pt0r/tiup/pkg/localdata"
	"github.com/c4pt0r/tiup/pkg/meta"
	"github.com/pingcap/errors"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

func newRunCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "run <component1>:[version]",
		Short: "Run a component of specific version",
		Long: `Run a specific version of a component. If no version number is specified,
the latest version installed locally will be run. If the specified
component does not have any version installed locally, the latest stable
version will be downloaded from the server. You can run the following
command if you want to have a try.

  # Quick start
  tiup run playground

  # Start a playground with a specified name
  tiup run playground --name p1`,
		FParseErrWhitelist: cobra.FParseErrWhitelist{
			UnknownFlags: true,
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			component, version := meta.ParseCompVersion(args[0])
			if !isSupportedComponent(component) {
				return fmt.Errorf("unkonwn component `%s` (see supported components via `tiup list --refresh`)", component)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			p, err := launchComponent(ctx, component, version, name, args[1:])
			// If the process has been launched, we must save the process info to meta directory
			if err == nil || (p != nil && p.Pid != 0) {
				metaFile := filepath.Join(p.Dir, localdata.MetaFilename)
				file, err := os.OpenFile(metaFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
				if err == nil {
					defer file.Close()
					encoder := json.NewEncoder(file)
					encoder.SetIndent("", "    ")
					_ = encoder.Encode(p)
				}
			}
			if err != nil {
				fmt.Printf("Failed to start component `%s`\n", component)
				return err
			}

			ch := make(chan error)
			go func() {
				defer close(ch)

				fmt.Printf("Starting %s %s \n", p.Exec, strings.Join(p.Args, " "))
				ch <- p.cmd.Wait()
			}()

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGQUIT)

			select {
			case s := <-sig:
				fmt.Printf("Got signal %v (Component: %v. PID: %v)\n", s, component, p.Pid)
				if component == "tidb" {
					return syscall.Kill(p.Pid, syscall.SIGKILL)
				}
				return syscall.Kill(p.Pid, s.(syscall.Signal))

			case err := <-ch:
				return errors.Annotatef(err, "start `%s` (wd:%s) failed", p.Exec, p.Dir)
			}
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", "Specify a name for this task")

	return cmd
}

func isSupportedComponent(component string) bool {
	// check local manifest
	manifest := profile.Manifest()
	if manifest != nil && manifest.HasComponent(component) {
		return true
	}

	manifest, err := repository.Manifest()
	if err != nil {
		fmt.Println("Fetch latest manifest error:", err)
		return false
	}
	if err := profile.SaveManifest(manifest); err != nil {
		fmt.Println("Save latest manifest error:", err)
	}
	return manifest.HasComponent(component)
}

type process struct {
	Component   string   `json:"component"`
	CreatedTime string   `json:"created_time"`
	Pid         int      `json:"pid"`            // PID of the process
	Exec        string   `json:"exec"`           // Path to the binary
	Args        []string `json:"args,omitempty"` // Command line arguments
	Env         []string `json:"env,omitempty"`  // Environment variables
	Dir         string   `json:"dir,omitempty"`  // Working directory
	cmd         *exec.Cmd
}

func base62Name() string {
	const base = 62
	const sets = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 0)
	num := time.Now().UnixNano()
	for num > 0 {
		r := math.Mod(float64(num), float64(base))
		num /= base
		b = append([]byte{sets[int(r)]}, b...)
	}
	return string(b)
}

func launchComponent(ctx context.Context, component string, version meta.Version, name string, args []string) (*process, error) {
	binPath, err := downloadIfMissing(component, version)
	if err != nil {
		return nil, err
	}

	wd := os.Getenv(localdata.EnvNameInstanceDataDir)
	if wd == "" {
		// Generate a name for current instance if the name doesn't specified
		if name == "" {
			name = base62Name()
		}
		wd = profile.Path(filepath.Join(localdata.DataParentDir, name))
	}

	if err := os.MkdirAll(wd, 0755); err != nil {
		return nil, err
	}

	envs := []string{
		fmt.Sprintf("%s=%s", localdata.EnvNameHome, profile.Root()),
		fmt.Sprintf("%s=%s", localdata.EnvNameInstanceDataDir, wd),
	}

	// init the command
	c := exec.CommandContext(ctx, binPath, args...)
	c.Env = append(
		os.Environ(),
		envs...,
	)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Dir = wd

	p := &process{
		Component:   component,
		CreatedTime: time.Now().Format(time.RFC3339),
		Exec:        binPath,
		Args:        args,
		Dir:         wd,
		Env:         envs,
		cmd:         c,
	}

	err = p.cmd.Start()
	if p.cmd.Process != nil {
		p.Pid = p.cmd.Process.Pid
	}
	return p, err
}

func downloadIfMissing(component string, version meta.Version) (string, error) {
	versions, err := profile.InstalledVersions(component)
	if err != nil {
		return "", err
	}

	// Use the latest version if user doesn't specify a specific version and
	// download the latest version if the specific component doesn't be installed

	// Check whether the specific version exist in local
	if version.IsEmpty() && len(versions) > 0 {
		sort.Slice(versions, func(i, j int) bool {
			return semver.Compare(versions[i], versions[j]) < 0
		})
		version = meta.Version(versions[len(versions)-1])
	}

	needDownload := version.IsEmpty()
	if !version.IsEmpty() {
		installed := false
		for _, v := range versions {
			if meta.Version(v) == version {
				installed = true
				break
			}
		}
		needDownload = !installed
	}

	if needDownload {
		fmt.Printf("The component `%s` doesn't installed, download from repository\n", component)
		manifest, err := repository.ComponentVersions(component)
		if err != nil {
			return "", errors.Trace(err)
		}
		err = profile.SaveVersions(component, manifest)
		if err != nil {
			return "", errors.Trace(err)
		}
		if version.IsEmpty() {
			version = manifest.LatestVersion()
		}
		compDir := profile.ComponentsDir()
		spec := fmt.Sprintf("%s:%s", component, version)
		err = repository.DownloadComponent(compDir, spec)
		if err != nil {
			return "", errors.Trace(err)
		}
	}

	return profile.BinaryPath(component, version)
}
