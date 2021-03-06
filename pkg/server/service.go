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
	"github.com/docker/docker/pkg/truncindex"
	"google.golang.org/grpc"

	contentapi "github.com/containerd/containerd/api/services/content"
	"github.com/containerd/containerd/api/services/execution"
	imagesapi "github.com/containerd/containerd/api/services/images"
	rootfsapi "github.com/containerd/containerd/api/services/rootfs"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/rootfs"
	contentservice "github.com/containerd/containerd/services/content"
	imagesservice "github.com/containerd/containerd/services/images"
	rootfsservice "github.com/containerd/containerd/services/rootfs"

	"github.com/kubernetes-incubator/cri-containerd/pkg/metadata"
	"github.com/kubernetes-incubator/cri-containerd/pkg/metadata/store"
	osinterface "github.com/kubernetes-incubator/cri-containerd/pkg/os"
	"github.com/kubernetes-incubator/cri-containerd/pkg/registrar"

	"k8s.io/kubernetes/pkg/kubelet/api/v1alpha1/runtime"
)

// TODO remove the underscores from the following imports as the services are
// implemented. "_" is being used to hold the reference to keep autocomplete
// from deleting them until referenced below.
// nolint: golint
import (
	_ "github.com/containerd/containerd/api/types/container"
	_ "github.com/containerd/containerd/api/types/descriptor"
	_ "github.com/containerd/containerd/api/types/mount"
	_ "github.com/opencontainers/image-spec/specs-go"
	_ "github.com/opencontainers/runtime-spec/specs-go"
)

// CRIContainerdService is the interface implement CRI remote service server.
type CRIContainerdService interface {
	runtime.RuntimeServiceServer
	runtime.ImageServiceServer
}

// criContainerdService implements CRIContainerdService.
type criContainerdService struct {
	// os is an interface for all required os operations.
	os osinterface.OS
	// rootDir is the directory for managing cri-containerd files.
	rootDir string
	// sandboxStore stores all sandbox metadata.
	sandboxStore metadata.SandboxStore
	// imageMetadataStore stores all image metadata.
	imageMetadataStore metadata.ImageMetadataStore
	// sandboxNameIndex stores all sandbox names and make sure each name
	// is unique.
	sandboxNameIndex *registrar.Registrar
	// sandboxIDIndex is trie tree for truncated id indexing, e.g. after an
	// id "abcdefg" is added, we could use "abcd" to identify the same thing
	// as long as there is no ambiguity.
	sandboxIDIndex *truncindex.TruncIndex
	// containerService is containerd container service client.
	containerService execution.ContainerServiceClient
	// contentIngester is the containerd service to ingest content into
	// content store.
	contentIngester content.Ingester
	// contentProvider is the containerd service to get content from
	// content store.
	contentProvider content.Provider
	// rootfsUnpacker is the containerd service to unpack image content
	// into snapshots.
	rootfsUnpacker rootfs.Unpacker
	// imageStoreService is the containerd service to store and track
	// image metadata.
	imageStoreService images.Store
}

// NewCRIContainerdService returns a new instance of CRIContainerdService
func NewCRIContainerdService(conn *grpc.ClientConn, rootDir string) CRIContainerdService {
	// TODO: Initialize different containerd clients.
	// TODO(random-liu): [P2] Recover from runtime state and metadata store.
	return &criContainerdService{
		os:                 osinterface.RealOS{},
		rootDir:            rootDir,
		sandboxStore:       metadata.NewSandboxStore(store.NewMetadataStore()),
		imageMetadataStore: metadata.NewImageMetadataStore(store.NewMetadataStore()),
		// TODO(random-liu): Register sandbox id/name for recovered sandbox.
		sandboxNameIndex:  registrar.NewRegistrar(),
		sandboxIDIndex:    truncindex.NewTruncIndex(nil),
		containerService:  execution.NewContainerServiceClient(conn),
		imageStoreService: imagesservice.NewStoreFromClient(imagesapi.NewImagesClient(conn)),
		contentIngester:   contentservice.NewIngesterFromClient(contentapi.NewContentClient(conn)),
		contentProvider:   contentservice.NewProviderFromClient(contentapi.NewContentClient(conn)),
		rootfsUnpacker:    rootfsservice.NewUnpackerFromClient(rootfsapi.NewRootFSClient(conn)),
	}
}
