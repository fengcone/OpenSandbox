// Copyright 2025 Alibaba Group Holding Ltd.
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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
)

const (
	pauseResumeNamespace = "default"
	registryServiceAddr  = "docker-registry.default.svc.cluster.local:5000"
	registryUsername     = "testuser"
	registryPassword     = "testpass"
)

var _ = Describe("PauseResume", Ordered, func() {
	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		if err != nil {
			Expect(err.Error()).To(ContainSubstring("AlreadyExists"))
		}

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("kubectl", "apply", "-f", "config/crd/bases")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("kubectl", "apply", "-k", "config/default")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("waiting for controller to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", namespace, "-o", "jsonpath={.items[0].status.phase}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"))
		}, 2*time.Minute).Should(Succeed())

		By("creating registry authentication secrets (before registry deployment)")
		err = createHtpasswdSecret(pauseResumeNamespace)
		Expect(err).NotTo(HaveOccurred())

		err = createDockerRegistrySecrets(pauseResumeNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("deploying Docker Registry")
		registryYAML, err := renderTemplate("testdata/registry-deployment.yaml", nil)
		Expect(err).NotTo(HaveOccurred())

		registryFile := filepath.Join("/tmp", "test-registry.yaml")
		err = os.WriteFile(registryFile, []byte(registryYAML), 0644)
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(registryFile)

		cmd = exec.Command("kubectl", "apply", "-f", registryFile)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for registry to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "deployment", "docker-registry",
				"-n", pauseResumeNamespace, "-o", "jsonpath={.status.availableReplicas}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("1"))
		}, 2*time.Minute).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up Docker Registry")
		cmd := exec.Command("kubectl", "delete", "deployment", "docker-registry", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "service", "docker-registry", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)

		By("cleaning up secrets")
		cmd = exec.Command("kubectl", "delete", "secret", "registry-auth", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "secret", "registry-push-secret", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "secret", "registry-pull-secret", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)

		By("cleaning up any remaining sandboxsnapshots")
		cmd = exec.Command("kubectl", "delete", "sandboxsnapshots", "--all", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)

		By("cleaning up any remaining batchsandboxes")
		cmd = exec.Command("kubectl", "delete", "batchsandboxes", "--all", "-n", pauseResumeNamespace, "--ignore-not-found=true")
		utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("kubectl", "delete", "-k", "config/default", "--ignore-not-found=true")
		utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("kubectl", "delete", "-f", "config/crd/bases", "--ignore-not-found=true")
		utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found=true")
		utils.Run(cmd)
	})

	Context("Pause", func() {
		It("should successfully pause a running sandbox", func() {
			const sandboxName = "test-pause-success"
			const snapshotName = "test-pause-success"

			By("creating BatchSandbox with pausePolicy")
			bsYAML, err := renderTemplate("testdata/batchsandbox-with-pause-policy.yaml", map[string]interface{}{
				"BatchSandboxName":          sandboxName,
				"Namespace":                 pauseResumeNamespace,
				"SandboxImage":              utils.SandboxImage,
				"SnapshotRegistry":          registryServiceAddr,
				"SnapshotPushSecretName":    "registry-push-secret",
				"ResumeImagePullSecretName": "registry-pull-secret",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-pause-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd := exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 2*time.Minute).Should(Succeed())

			By("getting pod info from BatchSandbox")
			cmd = exec.Command("kubectl", "get", "pods", "-n", pauseResumeNamespace, "-o", "json")
			podsJSON, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var podList struct {
				Items []struct {
					Metadata struct {
						Name            string `json:"name"`
						OwnerReferences []struct {
							Kind string `json:"kind"`
							Name string `json:"name"`
						} `json:"ownerReferences"`
					} `json:"metadata"`
					Spec struct {
						NodeName string `json:"nodeName"`
					} `json:"spec"`
				} `json:"items"`
			}
			err = json.Unmarshal([]byte(podsJSON), &podList)
			Expect(err).NotTo(HaveOccurred())

			// Find pods owned by this BatchSandbox
			var podName, nodeName string
			for _, pod := range podList.Items {
				for _, owner := range pod.Metadata.OwnerReferences {
					if owner.Kind == "BatchSandbox" && owner.Name == sandboxName {
						podName = pod.Metadata.Name
						nodeName = pod.Spec.NodeName
						break
					}
				}
				if podName != "" {
					break
				}
			}
			Expect(podName).NotTo(BeEmpty(), "Should find a pod owned by BatchSandbox")

			By("creating SandboxSnapshot CR")
			pausedAt := time.Now().UTC().Format(time.RFC3339)
			snapshotYAML, err := renderTemplate("testdata/sandboxsnapshot.yaml", map[string]interface{}{
				"SnapshotName":              snapshotName,
				"Namespace":                 pauseResumeNamespace,
				"SandboxId":                 sandboxName,
				"SourceBatchSandboxName":    sandboxName,
				"SourcePodName":             podName,
				"SourceNodeName":            nodeName,
				"ImageUri":                  fmt.Sprintf("%s/%s:snapshot", registryServiceAddr, sandboxName),
				"SnapshotPushSecretName":    "registry-push-secret",
				"ResumeImagePullSecretName": "registry-pull-secret",
				"SandboxImage":              utils.SandboxImage,
				"PausedAt":                  pausedAt,
			})
			Expect(err).NotTo(HaveOccurred())

			snapshotFile := filepath.Join("/tmp", "test-pause-snapshot.yaml")
			err = os.WriteFile(snapshotFile, []byte(snapshotYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(snapshotFile)

			cmd = exec.Command("kubectl", "apply", "-f", snapshotFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for SandboxSnapshot to be Ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sandboxsnapshot", snapshotName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Ready"))
			}, 2*time.Minute).Should(Succeed())

			By("verifying commit Job completed successfully")
			cmd = exec.Command("kubectl", "get", "job", fmt.Sprintf("%s-commit", snapshotName),
				"-n", pauseResumeNamespace, "-o", "jsonpath={.status.succeeded}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("1"))

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "sandboxsnapshot", snapshotName, "-n", pauseResumeNamespace)
			utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace)
			utils.Run(cmd)
		})
	})

	Context("Resume", func() {
		It("should successfully resume from snapshot", func() {
			const sandboxName = "test-resume-success"
			const snapshotName = "test-resume-success"

			// Get actual node name from the cluster
			cmd := exec.Command("kubectl", "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
			nodeName, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeName).NotTo(BeEmpty())

			By("creating a Ready SandboxSnapshot directly")
			pausedAt := time.Now().UTC().Format(time.RFC3339)
			snapshotYAML, err := renderTemplate("testdata/sandboxsnapshot.yaml", map[string]interface{}{
				"SnapshotName":              snapshotName,
				"Namespace":                 pauseResumeNamespace,
				"SandboxId":                 sandboxName,
				"SourceBatchSandboxName":    sandboxName,
				"SourcePodName":             "fake-pod",
				"SourceNodeName":            nodeName,
				"ImageUri":                  fmt.Sprintf("%s/%s:snapshot", registryServiceAddr, sandboxName),
				"SnapshotPushSecretName":    "registry-push-secret",
				"ResumeImagePullSecretName": "registry-pull-secret",
				"SandboxImage":              utils.SandboxImage,
				"PausedAt":                  pausedAt,
			})
			Expect(err).NotTo(HaveOccurred())

			snapshotFile := filepath.Join("/tmp", "test-resume-snapshot.yaml")
			err = os.WriteFile(snapshotFile, []byte(snapshotYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(snapshotFile)

			cmd = exec.Command("kubectl", "apply", "-f", snapshotFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for SandboxSnapshot to be Ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sandboxsnapshot", snapshotName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Ready"))
			}, 2*time.Minute).Should(Succeed())

			By("creating BatchSandbox with resumed-from-snapshot annotation")
			bsYAML := fmt.Sprintf(`
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: %s
  namespace: %s
  annotations:
    sandbox.opensandbox.io/resumed-from-snapshot: "true"
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: sandbox
        image: %s
        command: ["sh", "-c", "echo 'Resumed' && sleep 3600"]
`, sandboxName, pauseResumeNamespace, utils.SandboxImage)

			bsFile := filepath.Join("/tmp", "test-resume-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 2*time.Minute).Should(Succeed())

			By("verifying resumed-from-snapshot annotation")
			cmd = exec.Command("kubectl", "get", "batchsandbox", sandboxName,
				"-n", pauseResumeNamespace, "-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/resumed-from-snapshot}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("true"))

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace)
			utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "sandboxsnapshot", snapshotName, "-n", pauseResumeNamespace)
			utils.Run(cmd)
		})
	})

	Context("Cleanup", func() {
		It("should keep snapshot when deleting sandbox (snapshot is independent resource)", func() {
			const sandboxName = "test-cleanup"
			const snapshotName = "test-cleanup"

			// Get actual node name from the cluster
			cmd := exec.Command("kubectl", "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
			nodeName, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeName).NotTo(BeEmpty())

			By("creating BatchSandbox with pausePolicy")
			bsYAML, err := renderTemplate("testdata/batchsandbox-with-pause-policy.yaml", map[string]interface{}{
				"BatchSandboxName":          sandboxName,
				"Namespace":                 pauseResumeNamespace,
				"SandboxImage":              utils.SandboxImage,
				"SnapshotRegistry":          registryServiceAddr,
				"SnapshotPushSecretName":    "registry-push-secret",
				"ResumeImagePullSecretName": "registry-pull-secret",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-cleanup-bs.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be Running")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", sandboxName,
					"-n", pauseResumeNamespace, "-o", "jsonpath={.status.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 2*time.Minute).Should(Succeed())

			By("creating SandboxSnapshot CR")
			pausedAt := time.Now().UTC().Format(time.RFC3339)
			snapshotYAML, err := renderTemplate("testdata/sandboxsnapshot.yaml", map[string]interface{}{
				"SnapshotName":              snapshotName,
				"Namespace":                 pauseResumeNamespace,
				"SandboxId":                 sandboxName,
				"SourceBatchSandboxName":    sandboxName,
				"SourcePodName":             "fake-pod",
				"SourceNodeName":            nodeName,
				"ImageUri":                  fmt.Sprintf("%s/%s:snapshot", registryServiceAddr, sandboxName),
				"SnapshotPushSecretName":    "registry-push-secret",
				"ResumeImagePullSecretName": "registry-pull-secret",
				"SandboxImage":              utils.SandboxImage,
				"PausedAt":                  pausedAt,
			})
			Expect(err).NotTo(HaveOccurred())

			snapshotFile := filepath.Join("/tmp", "test-cleanup-snapshot.yaml")
			err = os.WriteFile(snapshotFile, []byte(snapshotYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(snapshotFile)

			cmd = exec.Command("kubectl", "apply", "-f", snapshotFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for SandboxSnapshot to exist")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sandboxsnapshot", snapshotName,
					"-n", pauseResumeNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 30*time.Second).Should(Succeed())

			By("deleting BatchSandbox")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", sandboxName, "-n", pauseResumeNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying SandboxSnapshot still exists (independent resource)")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sandboxsnapshot", snapshotName,
					"-n", pauseResumeNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 5*time.Second).Should(Succeed())

			By("cleaning up SandboxSnapshot manually")
			cmd = exec.Command("kubectl", "delete", "sandboxsnapshot", snapshotName, "-n", pauseResumeNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// createHtpasswdSecret creates the htpasswd secret for registry authentication.
func createHtpasswdSecret(namespace string) error {
	// Use openssl to generate htpasswd entry
	cmd := exec.Command("sh", "-c", fmt.Sprintf("echo '%s:$(openssl passwd -apr1 %s)'", registryUsername, registryPassword))
	output, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to generate htpasswd: %w", err)
	}

	// Write to temp file
	tmpFile := filepath.Join(os.TempDir(), "htpasswd")
	if err := os.WriteFile(tmpFile, []byte(output), 0644); err != nil {
		return fmt.Errorf("failed to write htpasswd file: %w", err)
	}
	defer os.Remove(tmpFile)

	// Create secret
	cmd = exec.Command("kubectl", "create", "secret", "generic", "registry-auth",
		"--from-file=htpasswd="+tmpFile, "-n", namespace)
	if _, err := utils.Run(cmd); err != nil {
		// Try to delete and recreate
		cmd = exec.Command("kubectl", "delete", "secret", "registry-auth", "-n", namespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "secret", "generic", "registry-auth",
			"--from-file=htpasswd="+tmpFile, "-n", namespace)
		if _, err := utils.Run(cmd); err != nil {
			return fmt.Errorf("failed to create registry-auth secret: %w", err)
		}
	}

	return nil
}

// createDockerRegistrySecrets creates docker-registry secrets for push/pull.
func createDockerRegistrySecrets(namespace string) error {
	server := registryServiceAddr

	// Create push secret
	cmd := exec.Command("kubectl", "create", "secret", "docker-registry", "registry-push-secret",
		"--docker-server="+server,
		"--docker-username="+registryUsername,
		"--docker-password="+registryPassword,
		"-n", namespace)
	if _, err := utils.Run(cmd); err != nil {
		cmd = exec.Command("kubectl", "delete", "secret", "registry-push-secret", "-n", namespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "secret", "docker-registry", "registry-push-secret",
			"--docker-server="+server,
			"--docker-username="+registryUsername,
			"--docker-password="+registryPassword,
			"-n", namespace)
		if _, err := utils.Run(cmd); err != nil {
			return fmt.Errorf("failed to create registry-push-secret: %w", err)
		}
	}

	// Create pull secret
	cmd = exec.Command("kubectl", "create", "secret", "docker-registry", "registry-pull-secret",
		"--docker-server="+server,
		"--docker-username="+registryUsername,
		"--docker-password="+registryPassword,
		"-n", namespace)
	if _, err := utils.Run(cmd); err != nil {
		cmd = exec.Command("kubectl", "delete", "secret", "registry-pull-secret", "-n", namespace, "--ignore-not-found=true")
		utils.Run(cmd)
		cmd = exec.Command("kubectl", "create", "secret", "docker-registry", "registry-pull-secret",
			"--docker-server="+server,
			"--docker-username="+registryUsername,
			"--docker-password="+registryPassword,
			"-n", namespace)
		if _, err := utils.Run(cmd); err != nil {
			return fmt.Errorf("failed to create registry-pull-secret: %w", err)
		}
	}

	return nil
}
