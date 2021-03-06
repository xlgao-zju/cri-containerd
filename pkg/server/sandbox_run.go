/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"syscall"
	"time"

	prototypes "github.com/gogo/protobuf/types"
	"github.com/golang/glog"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"golang.org/x/net/context"

	"github.com/containerd/containerd/api/services/execution"
	"github.com/containerd/containerd/api/types/mount"

	"k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/runtime"

	"github.com/kubernetes-incubator/cri-containerd/pkg/metadata"
)

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes should ensure
// the sandbox is in ready state.
func (c *criContainerdService) RunPodSandbox(ctx context.Context, r *runtime.RunPodSandboxRequest) (retRes *runtime.RunPodSandboxResponse, retErr error) {
	glog.V(2).Infof("RunPodSandbox with config %+v", r.GetConfig())
	defer func() {
		if retErr == nil {
			glog.V(2).Infof("RunPodSandbox returns sandbox id %q", retRes.GetPodSandboxId())
		}
	}()

	config := r.GetConfig()

	// Generate unique id and name for the sandbox and reserve the name.
	id := generateID()
	name := makeSandboxName(config.GetMetadata())
	// Reserve the sandbox name to avoid concurrent `RunPodSandbox` request starting the
	// same sandbox.
	if err := c.sandboxNameIndex.Reserve(name, id); err != nil {
		return nil, fmt.Errorf("failed to reserve sandbox name %q: %v", name, err)
	}
	defer func() {
		// Release the name if the function returns with an error.
		if retErr != nil {
			c.sandboxNameIndex.ReleaseByName(name)
		}
	}()
	// Register the sandbox id.
	if err := c.sandboxIDIndex.Add(id); err != nil {
		return nil, fmt.Errorf("failed to insert sandbox id %q: %v", id, err)
	}
	defer func() {
		// Delete the sandbox id if the function returns with an error.
		if retErr != nil {
			c.sandboxIDIndex.Delete(id) // nolint: errcheck
		}
	}()

	// Create initial sandbox metadata.
	meta := metadata.SandboxMetadata{
		ID:     id,
		Name:   name,
		Config: config,
	}

	// TODO(random-liu): [P0] Ensure pause image snapshot, apply default image config
	// and get snapshot mounts.
	// Use fixed rootfs path and sleep command.
	const rootPath = "/"

	// TODO(random-liu): [P0] Set up sandbox network with network plugin.

	// Create sandbox container root directory.
	// Prepare streaming named pipe.
	sandboxRootDir := getSandboxRootDir(c.rootDir, id)
	if err := c.os.MkdirAll(sandboxRootDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create sandbox root directory %q: %v",
			sandboxRootDir, err)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the sandbox root directory.
			if err := c.os.RemoveAll(sandboxRootDir); err != nil {
				glog.Errorf("Failed to remove sandbox root directory %q: %v",
					sandboxRootDir, err)
			}
		}
	}()

	// TODO(random-liu): [P1] Moving following logging related logic into util functions.
	// Discard sandbox container output because we don't care about it.
	_, stdout, stderr := getStreamingPipes(sandboxRootDir)
	for _, p := range []string{stdout, stderr} {
		f, err := c.os.OpenFifo(ctx, p, syscall.O_RDONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0700)
		if err != nil {
			return nil, fmt.Errorf("failed to open named pipe %q: %v", p, err)
		}
		defer func(c io.Closer) {
			if retErr != nil {
				c.Close()
			}
		}(f)
		go func(r io.ReadCloser) {
			// Discard the output for now.
			io.Copy(ioutil.Discard, r) // nolint: errcheck
			r.Close()
		}(f)
	}

	// Start sandbox container.
	spec := c.generateSandboxContainerSpec(id, config)
	rawSpec, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal oci spec %+v: %v", spec, err)
	}
	glog.V(4).Infof("Sandbox container spec: %+v", spec)
	createOpts := &execution.CreateRequest{
		ID: id,
		Spec: &prototypes.Any{
			TypeUrl: runtimespec.Version,
			Value:   rawSpec,
		},
		// TODO(random-liu): [P0] Get rootfs mount from containerd.
		Rootfs: []*mount.Mount{
			{
				Type:   "bind",
				Source: rootPath,
				Options: []string{
					"rw",
					"rbind",
				},
			},
		},
		Runtime: defaultRuntime,
		// No stdin for sandbox container.
		Stdout: stdout,
		Stderr: stderr,
	}

	// Create sandbox container in containerd.
	glog.V(5).Infof("Create sandbox container (id=%q, name=%q) with options %+v.",
		id, name, createOpts)
	createResp, err := c.containerService.Create(ctx, createOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox container %q: %v",
			id, err)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the sandbox container if an error is returned.
			if _, err := c.containerService.Delete(ctx, &execution.DeleteRequest{ID: id}); err != nil {
				glog.Errorf("Failed to delete sandbox container %q: %v",
					id, err)
			}
		}
	}()

	// Start sandbox container in containerd.
	if _, err := c.containerService.Start(ctx, &execution.StartRequest{ID: id}); err != nil {
		return nil, fmt.Errorf("failed to start sandbox container %q: %v",
			id, err)
	}

	// Add sandbox into sandbox store.
	meta.CreatedAt = time.Now().UnixNano()
	// TODO(random-liu): [P2] Replace with permanent network namespace.
	meta.NetNS = getNetworkNamespace(createResp.Pid)
	if err := c.sandboxStore.Create(meta); err != nil {
		return nil, fmt.Errorf("failed to add sandbox metadata %+v into store: %v",
			meta, err)
	}

	return &runtime.RunPodSandboxResponse{PodSandboxId: id}, nil
}

func (c *criContainerdService) generateSandboxContainerSpec(id string, config *runtime.PodSandboxConfig) *runtimespec.Spec {
	// TODO(random-liu): [P0] Get command from image config.
	pauseCommand := []string{"sh", "-c", "while true; do sleep 1000000000; done"}

	// Creates a spec Generator with the default spec.
	// TODO(random-liu): [P1] Compare the default settings with docker and containerd default.
	g := generate.New()

	// Set relative root path.
	g.SetRootPath(relativeRootfsPath)

	// Set process commands.
	g.SetProcessArgs(pauseCommand)

	// Make root of sandbox container read-only.
	g.SetRootReadonly(true)

	// Set hostname.
	g.SetHostname(config.GetHostname())

	// TODO(random-liu): [P0] Set DNS options. Maintain a resolv.conf for the sandbox.

	// TODO(random-liu): [P0] Add NamespaceGetter and PortMappingGetter to initialize network plugin.

	// TODO(random-liu): [P0] Add annotation to identify the container is managed by cri-containerd.
	// TODO(random-liu): [P2] Consider whether to add labels and annotations to the container.

	// Set cgroups parent.
	if config.GetLinux().GetCgroupParent() != "" {
		cgroupsPath := getCgroupsPath(config.GetLinux().GetCgroupParent(), id)
		g.SetLinuxCgroupsPath(cgroupsPath)
	}
	// When cgroup parent is not set, containerd-shim will create container in a child cgroup
	// of the cgroup itself is in.
	// TODO(random-liu): [P2] Set default cgroup path if cgroup parent is not specified.

	// Set namespace options.
	nsOptions := config.GetLinux().GetSecurityContext().GetNamespaceOptions()
	// TODO(random-liu): [P1] Create permanent network namespace, so that we could still cleanup
	// network namespace after sandbox container dies unexpectedly.
	// By default, all namespaces are enabled for the container, runc will create a new namespace
	// for it. By removing the namespace, the container will inherit the namespace of the runtime.
	if nsOptions.GetHostNetwork() {
		g.RemoveLinuxNamespace(string(runtimespec.NetworkNamespace)) // nolint: errcheck
		// TODO(random-liu): [P1] Figure out how to handle UTS namespace.
	}

	if nsOptions.GetHostPid() {
		g.RemoveLinuxNamespace(string(runtimespec.PIDNamespace)) // nolint: errcheck
	}

	// TODO(random-liu): [P0] Deal with /dev/shm. Use host for HostIpc, and create and mount for
	// non-HostIpc. What about mqueue?
	if nsOptions.GetHostIpc() {
		g.RemoveLinuxNamespace(string(runtimespec.IPCNamespace)) // nolint: errcheck
	}

	// TODO(random-liu): [P1] Apply SeLinux options.

	// TODO(random-liu): [P1] Set user.

	// TODO(random-liu): [P1] Set supplemental group.

	// TODO(random-liu): [P1] Set privileged.

	// TODO(random-liu): [P2] Set sysctl from annotations.

	// TODO(random-liu): [P2] Set apparmor and seccomp from annotations.

	// TODO(random-liu): [P1] Set default sandbox container resource limit.

	return g.Spec()
}
