// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/sofatutor/standalone-soci-indexer/utils/log"
	registryutils "github.com/sofatutor/standalone-soci-indexer/utils/registry"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/content/local"
	"github.com/containerd/containerd/images"
	"oras.land/oras-go/v2/content/oci"

	"github.com/awslabs/soci-snapshotter/soci"
	"github.com/awslabs/soci-snapshotter/soci/store"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	BuildFailedMessage          = "SOCI index build error"
	PushFailedMessage           = "SOCI index push error"
	SkipPushOnEmptyIndexMessage = "Skipping pushing SOCI index as it does not contain any zTOCs"
	BuildAndPushSuccessMessage  = "Successfully built and pushed SOCI v2 index"

	artifactsStoreName = "store"
	artifactsDbName    = "artifacts.db"
)

func indexAndPush(ctx context.Context, repo string, digest string, registryUrl string, authToken string) (string, error) {
	ctx = context.WithValue(ctx, "RegistryURL", registryUrl)

	registry, err := registryutils.Init(ctx, registryUrl, authToken)
	if err != nil {
		return logAndReturnError(ctx, "Remote registry initialization error", err)
	}

	err = registry.ValidateImageManifest(ctx, repo, digest)
	if err != nil {
		log.Warn(ctx, fmt.Sprintf("Image manifest validation error: %v", err))
		// Returning a non error to skip retries
		return "Exited early due to manifest validation error", nil
	}

	// Directory in lambda storage to store images and SOCI artifacts
	dataDir, err := createTempDir(ctx)
	if err != nil {
		return logAndReturnError(ctx, "Directory create error", err)
	}
	defer cleanUp(ctx, dataDir)

	sociStore, err := initSociStore(ctx, dataDir)
	if err != nil {
		return logAndReturnError(ctx, "OCI storage initialization error", err)
	}

	desc, err := registry.Pull(ctx, repo, sociStore, digest)
	if err != nil {
		return logAndReturnError(ctx, "Image pull error", err)
	}

	image := images.Image{
		Name:   repo + "@" + digest,
		Target: *desc,
	}

	ociIndexDesc, err := buildIndex(ctx, dataDir, sociStore, image)
	if err != nil {
		if errors.Is(err, soci.ErrEmptyIndex) {
			log.Warn(ctx, SkipPushOnEmptyIndexMessage)
			return SkipPushOnEmptyIndexMessage, nil
		}
		return logAndReturnError(ctx, BuildFailedMessage, err)
	}
	ctx = context.WithValue(ctx, "SOCIIndexDigest", ociIndexDesc.Digest.String())

	err = registry.Push(ctx, sociStore, *ociIndexDesc, repo)
	if err != nil {
		return logAndReturnError(ctx, PushFailedMessage, err)
	}

	log.Info(ctx, BuildAndPushSuccessMessage)
	return BuildAndPushSuccessMessage, nil
}

// Create a temp directory in /tmp
// The directory is prefixed by the Lambda's request id
func createTempDir(ctx context.Context) (string, error) {
	log.Info(ctx, "Creating a directory to store images and SOCI artifacts")
	tempDir, err := os.MkdirTemp("/tmp", "soci") // The temp dir name is prefixed by the request id
	return tempDir, err
}

// Clean up the data written by the Lambda
func cleanUp(ctx context.Context, dataDir string) {
	log.Info(ctx, fmt.Sprintf("Removing all files in %s", dataDir))
	if err := os.RemoveAll(dataDir); err != nil {
		log.Error(ctx, "Clean up error", err)
	}
}

// Init containerd store — shares the same blob directory as the OCI store so that
// the SOCI index builder can read image layers pulled via oras.
func initContainerdStore(dataDir string) (content.Store, error) {
	return local.NewStore(path.Join(dataDir, artifactsStoreName))
}

// Init SOCI artifact store
func initSociStore(ctx context.Context, dataDir string) (*store.SociStore, error) {
	ociStore, err := oci.NewWithContext(ctx, path.Join(dataDir, artifactsStoreName))
	return &store.SociStore{Store: ociStore}, err
}

// Init a new instance of SOCI artifacts DB
func initSociArtifactsDb(dataDir string) (*soci.ArtifactsDb, error) {
	artifactsDbPath := path.Join(dataDir, artifactsDbName)
	artifactsDb, err := soci.NewDB(artifactsDbPath)
	if err != nil {
		return nil, err
	}
	return artifactsDb, nil
}

// buildIndex creates a SOCI v2 index for the image and wraps it together with
// the (annotated) image manifest into an OCI image index. The returned descriptor
// is the OCI image index that should be pushed to the registry.
func buildIndex(ctx context.Context, dataDir string, sociStore *store.SociStore, image images.Image) (*ocispec.Descriptor, error) {
	log.Info(ctx, "Building SOCI v2 index")

	artifactsDb, err := initSociArtifactsDb(dataDir)
	if err != nil {
		return nil, err
	}

	containerdStore, err := initContainerdStore(dataDir)
	if err != nil {
		return nil, err
	}

	builder, err := soci.NewIndexBuilder(containerdStore, sociStore,
		soci.WithArtifactsDb(artifactsDb),
		soci.WithBuildToolIdentifier("standalone-soci-indexer"),
	)
	if err != nil {
		return nil, err
	}

	// Convert builds the SOCI v2 index and wraps the image manifest + SOCI index
	// into an OCI image index. For single-platform images it automatically detects
	// the platform from the image config.
	ociIndexDesc, err := builder.Convert(ctx, image)
	if err != nil {
		return nil, err
	}

	return ociIndexDesc, nil
}

// Log and return error
func logAndReturnError(ctx context.Context, msg string, err error) (string, error) {
	log.Error(ctx, msg, err)
	return msg, err
}
