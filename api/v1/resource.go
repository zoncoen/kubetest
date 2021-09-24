//go:build !ignore_autogenerated
// +build !ignore_autogenerated

package v1

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/client-go/kubernetes"
)

type ResourceManager struct {
	repoMgr     *RepositoryManager
	tokenMgr    *TokenManager
	artifactMgr *ArtifactManager
	setupOnce   sync.Once
	doneSetup   bool
}

func NewResourceManager(clientset *kubernetes.Clientset, testjob TestJob) *ResourceManager {
	tokenClient := NewTokenClient(clientset, testjob.Namespace)
	tokenMgr := NewTokenManager(testjob.Spec.Tokens, tokenClient)
	repoMgr := NewRepositoryManager(testjob.Spec.Repos, tokenMgr)
	artifactMgr := NewArtifactManager(testjob.Spec.ExportArtifacts)
	return &ResourceManager{
		repoMgr:     repoMgr,
		tokenMgr:    tokenMgr,
		artifactMgr: artifactMgr,
	}
}

func (m *ResourceManager) Cleanup() error {
	return m.repoMgr.Cleanup()
}

func (m *ResourceManager) Setup(ctx context.Context) error {
	defer func() {
		m.doneSetup = true
	}()
	var err error
	m.setupOnce.Do(func() {
		err = m.repoMgr.CloneAll(ctx)
	})
	return err
}

func (m *ResourceManager) RepositoryPathByName(name string) (string, error) {
	if !m.doneSetup {
		return "", fmt.Errorf("kubetest: resource manager isn't setup")
	}
	return m.repoMgr.ClonedPathByRepoName(name)
}

func (m *ResourceManager) TokenPathByName(ctx context.Context, name string) (string, error) {
	if !m.doneSetup {
		return "", fmt.Errorf("kubetest: resource manager isn't setup")
	}
	token, err := m.tokenMgr.TokenByName(ctx, name)
	if err != nil {
		return "", err
	}
	return token.File, nil
}

func (m *ResourceManager) ArtifactPathByName(name string) (string, error) {
	if !m.doneSetup {
		return "", fmt.Errorf("kubetest: resource manager isn't setup")
	}
	return m.artifactMgr.LocalPathByName(name)
}

func (m *ResourceManager) ExportArtifacts() error {
	return m.artifactMgr.ExportArtifacts()
}
