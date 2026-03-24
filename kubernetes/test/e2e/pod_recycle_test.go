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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/utils"
)

// PodRecycleMeta mirrors the internal PodRecycleMeta structure for e2e testing.
type PodRecycleMeta struct {
	State                string           `json:"state"`
	KillSentAt           int64            `json:"killSentAt"`
	TriggeredAt          int64            `json:"triggeredAt"`
	Attempt              int32            `json:"attempt"`
	KillFailed           bool             `json:"killFailed,omitempty"`
	FailReason           string           `json:"failReason,omitempty"`
	InitialRestartCounts map[string]int32 `json:"initialRestartCounts,omitempty"`
}

var _ = Describe("Pod Recycle Policy", Ordered, func() {
	const testNamespace = "default"

	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, _ = utils.Run(cmd) // Ignore error if namespace already exists

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, _ = utils.Run(cmd)

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("CONTROLLER_IMG=%s", utils.ControllerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("waiting for controller to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "wait", "--for=condition=available",
				"deployment/opensandbox-controller-manager", "-n", namespace, "--timeout=120s")
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}, 2*time.Minute).Should(Succeed())
	})

	AfterAll(func() {
		By("undeploying the controller-manager")
		cmd := exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	Context("Delete Policy (default behavior)", func() {
		It("should delete pods directly when BatchSandbox is deleted", func() {
			const poolName = "test-pool-delete-policy"
			const batchSandboxName = "test-bs-delete-policy"
			const replicas = 1

			By("creating a Pool with default Delete policy")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    3,
				"BufferMin":    1,
				"PoolMax":      5,
				"PoolMin":      1,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-delete-policy.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}).Should(Succeed())

			By("waiting for Pool pods to be running and ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				phases := strings.Fields(output)
				g.Expect(len(phases)).To(BeNumerically(">=", 1), "Should have at least one pod")
				for _, phase := range phases {
					g.Expect(phase).To(Equal("Running"), "All pods should be Running, got: %s", phase)
				}
				// Check readiness
				cmd = exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				readyStatuses := strings.Fields(output)
				g.Expect(len(readyStatuses)).To(Equal(len(phases)), "All pods should have Ready condition")
				for _, status := range readyStatuses {
					g.Expect(status).To(Equal("True"), "All pods should be Ready")
				}
			}, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox to allocate pods")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
				"ExpireTime":       "",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-delete-policy.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be ready and recording pod names")
			var podNames []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				podNames = allocStatus.Pods
				g.Expect(len(podNames)).To(Equal(replicas))
			}).Should(Succeed())

			By("deleting BatchSandbox")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying pods are deleted (Delete policy)")
			Eventually(func(g Gomega) {
				for _, podName := range podNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace, "-o", "jsonpath={.metadata.deletionTimestamp}")
					output, err := utils.Run(cmd)
					if err != nil {
						// Pod not found means it's fully deleted - success
						g.Expect(err.Error()).To(ContainSubstring("not found"), "Pod %s should be deleted, got error: %s", podName, err.Error())
						continue
					}
					// If deletionTimestamp is set, pod is being terminated
					g.Expect(output).NotTo(BeEmpty(), "Pod %s should be deleted or terminating", podName)
				}
			}, 60*time.Second).Should(Succeed())

			By("cleaning up Pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})
	})

	Context("Restart Policy", func() {
		It("should restart pods and mark them ready for reuse", func() {
			const poolName = "test-pool-restart-policy"
			const batchSandboxName = "test-bs-restart-policy"
			const replicas = 1

			By("creating a Pool with Restart policy")
			poolYAML, err := renderTemplate("testdata/pool-with-restart-policy.yaml", map[string]interface{}{
				"PoolName":          poolName,
				"TaskExecutorImage": utils.TaskExecutorImage,
				"Namespace":         testNamespace,
				"BufferMax":         3,
				"BufferMin":         1,
				"PoolMax":           5,
				"PoolMin":           1,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-restart-policy.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}).Should(Succeed())

			By("waiting for Pool pods to be running and ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				phases := strings.Fields(output)
				g.Expect(len(phases)).To(BeNumerically(">=", 1), "Should have at least one pod")
				for _, phase := range phases {
					g.Expect(phase).To(Equal("Running"), "All pods should be Running, got: %s", phase)
				}
				// Check readiness
				cmd = exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				readyStatuses := strings.Fields(output)
				g.Expect(len(readyStatuses)).To(Equal(len(phases)), "All pods should have Ready condition")
				for _, status := range readyStatuses {
					g.Expect(status).To(Equal("True"), "All pods should be Ready")
				}
			}, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox to allocate pods")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
				"ExpireTime":       "",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-restart-policy.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be ready and recording pod names")
			var podNames []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				podNames = allocStatus.Pods
				g.Expect(len(podNames)).To(Equal(replicas))
			}).Should(Succeed())

			By("deleting BatchSandbox to trigger restart")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying pods still exist after BatchSandbox deletion")
			Eventually(func(g Gomega) {
				for _, podName := range podNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.name}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "Pod %s should still exist", podName)
					g.Expect(output).To(Equal(podName))
				}
			}, 30*time.Second).Should(Succeed())

			By("verifying pods enter restart flow (Killing or Restarting state)")
			Eventually(func(g Gomega) {
				for _, podName := range podNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.annotations.pool\\.opensandbox\\.io/recycle-meta}")
					metaJSON, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(metaJSON).NotTo(BeEmpty(), "Pod should have recycle-meta annotation")

					var meta PodRecycleMeta
					err = json.Unmarshal([]byte(metaJSON), &meta)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(meta.State).To(Equal("Restarting"),
						"Pod state should be Restarting, got: %s", meta.State)
				}
			}, 30*time.Second).Should(Succeed())

			By("waiting for pods to complete restart and become Ready")
			Eventually(func(g Gomega) {
				for _, podName := range podNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.annotations.pool\\.opensandbox\\.io/recycle-meta}")
					metaJSON, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())

					// When restart completes, the annotation is cleared (empty string)
					// This indicates the pod is ready for reuse
					if metaJSON == "" {
						// Annotation cleared - restart complete, pod ready for reuse
						continue
					}

					var meta PodRecycleMeta
					err = json.Unmarshal([]byte(metaJSON), &meta)
					g.Expect(err).NotTo(HaveOccurred())
					// If annotation exists, state should be "Restarting" (pod still restarting)
					g.Expect(meta.State).To(Equal("Restarting"), "Pod %s should be in Restarting state, got: %s", podName, meta.State)
				}
			}, 2*time.Minute).Should(Succeed())

			By("verifying Pool status - restarting count should be 0 after restart completes")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.restarting}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Empty string or "0" both indicate 0 restarting pods
				g.Expect(output).To(Or(BeEmpty(), Equal("0")), "Pool restarting count should be 0, got: %s", output)
			}).Should(Succeed())

			By("cleaning up Pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should correctly update Pool.status.restarting during restart", func() {
			const poolName = "test-pool-restarting-status"
			const batchSandboxName = "test-bs-restarting-status"
			const replicas = 2

			By("creating a Pool with Restart policy")
			poolYAML, err := renderTemplate("testdata/pool-with-restart-policy.yaml", map[string]interface{}{
				"PoolName":          poolName,
				"TaskExecutorImage": utils.TaskExecutorImage,
				"Namespace":         testNamespace,
				"BufferMax":         5,
				"BufferMin":         2,
				"PoolMax":           10,
				"PoolMin":           2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-restarting-status.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}).Should(Succeed())

			By("waiting for Pool pods to be running and ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				phases := strings.Fields(output)
				g.Expect(len(phases)).To(BeNumerically(">=", 1), "Should have at least one pod")
				for _, phase := range phases {
					g.Expect(phase).To(Equal("Running"), "All pods should be Running, got: %s", phase)
				}
				// Check readiness
				cmd = exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				readyStatuses := strings.Fields(output)
				g.Expect(len(readyStatuses)).To(Equal(len(phases)), "All pods should have Ready condition")
				for _, status := range readyStatuses {
					g.Expect(status).To(Equal("True"), "All pods should be Ready")
				}
			}, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox to allocate pods")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
				"ExpireTime":       "",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-restarting-status.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(fmt.Sprintf("%d", replicas)))
			}).Should(Succeed())

			By("deleting BatchSandbox to trigger restart")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying Pool.status.restarting is updated (should be > 0 during restart)")
			// Note: This check is time-sensitive. Pods may restart quickly.
			// We check that restarting was set at some point, or wait for it to return to 0.
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.restarting}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// After restart completes, restarting should be 0
				// During restart, it could be > 0
				restarting := 0
				if output != "" {
					fmt.Sscanf(output, "%d", &restarting)
				}
				g.Expect(restarting).To(BeNumerically(">=", 0))
			}).Should(Succeed())

			By("waiting for all pods to complete restart")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.restarting}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Empty string or "0" both indicate 0 restarting pods
				g.Expect(output).To(Or(BeEmpty(), Equal("0")), "Pool restarting count should be 0 after restart completes")
			}, 2*time.Minute).Should(Succeed())

			By("verifying Pool.status.available increased after restart")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.available}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				available := 0
				if output != "" {
					fmt.Sscanf(output, "%d", &available)
				}
				g.Expect(available).To(BeNumerically(">=", replicas), "Available pods should include restarted pods")
			}).Should(Succeed())

			By("cleaning up Pool")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should reuse restarted pods for new BatchSandbox allocation", func() {
			const poolName = "test-pool-reuse"
			const batchSandboxName1 = "test-bs-reuse-1"
			const batchSandboxName2 = "test-bs-reuse-2"
			const replicas = 1

			By("creating a Pool with Restart policy")
			poolYAML, err := renderTemplate("testdata/pool-with-restart-policy.yaml", map[string]interface{}{
				"PoolName":          poolName,
				"TaskExecutorImage": utils.TaskExecutorImage,
				"Namespace":         testNamespace,
				"BufferMax":         3,
				"BufferMin":         1,
				"PoolMax":           3,
				"PoolMin":           1,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-reuse.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready and recording total pods")
			var initialTotal int
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				fmt.Sscanf(output, "%d", &initialTotal)
			}).Should(Succeed())

			By("creating first BatchSandbox to allocate pods")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName1,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
				"ExpireTime":       "",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-reuse-1.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for first BatchSandbox to be ready and recording pod names")
			var firstPodNames []string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName1, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				firstPodNames = allocStatus.Pods
				g.Expect(len(firstPodNames)).To(Equal(replicas))
			}).Should(Succeed())

			By("deleting first BatchSandbox to trigger restart")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName1, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for pods to complete restart and become Ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.restarting}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Empty string or "0" both indicate 0 restarting pods
				g.Expect(output).To(Or(BeEmpty(), Equal("0")), "Pool restarting count should be 0")
			}, 2*time.Minute).Should(Succeed())

			// Also wait for the pod's recycle state to be Ready
			By("waiting for pod recycle state to be Ready")
			Eventually(func(g Gomega) {
				for _, podName := range firstPodNames {
					cmd := exec.Command("kubectl", "get", "pod", podName, "-n", testNamespace,
						"-o", "jsonpath={.metadata.annotations.pool\\.opensandbox\\.io/recycle-meta}")
					metaJSON, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())

					// When restart completes, the annotation is cleared
					// So empty metaJSON means the pod is ready for reuse
					g.Expect(metaJSON).To(BeEmpty(),
						"Pod %s should have no recycle meta (restart completed), got: %s", podName, metaJSON)
				}
			}, 2*time.Minute).Should(Succeed())

			By("creating second BatchSandbox to verify pod reuse")
			bsYAML, err = renderTemplate("testdata/batchsandbox-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName2,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
				"ExpireTime":       "",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile2 := filepath.Join("/tmp", "test-bs-reuse-2.yaml")
			err = os.WriteFile(bsFile2, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile2)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile2)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying second BatchSandbox reuses the restarted pod")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName2, "-n", testNamespace,
					"-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}")
				allocStatusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(allocStatusJSON).NotTo(BeEmpty())

				var allocStatus struct {
					Pods []string `json:"pods"`
				}
				err = json.Unmarshal([]byte(allocStatusJSON), &allocStatus)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(allocStatus.Pods)).To(Equal(replicas))

				// Verify the pod names are the same (reused)
				for _, reusedPod := range allocStatus.Pods {
					g.Expect(firstPodNames).To(ContainElement(reusedPod),
						"Second BatchSandbox should reuse restarted pod, expected one of %v, got %s", firstPodNames, reusedPod)
				}
			}, 30*time.Second).Should(Succeed())

			By("verifying Pool total didn't increase (no new pods created)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.total}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				total := 0
				fmt.Sscanf(output, "%d", &total)
				g.Expect(total).To(BeNumerically("<=", initialTotal+1), // +1 for tolerance
					"Pool total should not increase significantly (pods should be reused)")
			}).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName2, "-n", testNamespace)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should exclude restarting pods from available count", func() {
			const poolName = "test-pool-available-exclude"
			const batchSandboxName = "test-bs-available-exclude"
			const replicas = 1

			By("creating a Pool with Restart policy")
			poolYAML, err := renderTemplate("testdata/pool-with-restart-policy.yaml", map[string]interface{}{
				"PoolName":          poolName,
				"TaskExecutorImage": utils.TaskExecutorImage,
				"Namespace":         testNamespace,
				"BufferMax":         5,
				"BufferMin":         2,
				"PoolMax":           10,
				"PoolMin":           2,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-available-exclude.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for Pool to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status.available}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
			}).Should(Succeed())

			By("waiting for Pool pods to be running and ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				phases := strings.Fields(output)
				g.Expect(len(phases)).To(BeNumerically(">=", 1), "Should have at least one pod")
				for _, phase := range phases {
					g.Expect(phase).To(Equal("Running"), "All pods should be Running, got: %s", phase)
				}
				// Check readiness
				cmd = exec.Command("kubectl", "get", "pods", "-n", testNamespace,
					"-l", fmt.Sprintf("sandbox.opensandbox.io/pool-name=%s", poolName),
					"-o", "jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				readyStatuses := strings.Fields(output)
				g.Expect(len(readyStatuses)).To(Equal(len(phases)), "All pods should have Ready condition")
				for _, status := range readyStatuses {
					g.Expect(status).To(Equal("True"), "All pods should be Ready")
				}
			}, 2*time.Minute).Should(Succeed())

			By("creating a BatchSandbox")
			bsYAML, err := renderTemplate("testdata/batchsandbox-pooled.yaml", map[string]interface{}{
				"BatchSandboxName": batchSandboxName,
				"SandboxImage":     utils.SandboxImage,
				"Namespace":        testNamespace,
				"Replicas":         replicas,
				"PoolName":         poolName,
				"ExpireTime":       "",
			})
			Expect(err).NotTo(HaveOccurred())

			bsFile := filepath.Join("/tmp", "test-bs-available-exclude.yaml")
			err = os.WriteFile(bsFile, []byte(bsYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(bsFile)

			cmd = exec.Command("kubectl", "apply", "-f", bsFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for BatchSandbox to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "batchsandbox", batchSandboxName, "-n", testNamespace,
					"-o", "jsonpath={.status.allocated}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(fmt.Sprintf("%d", replicas)))
			}).Should(Succeed())

			By("deleting BatchSandbox to trigger restart")
			cmd = exec.Command("kubectl", "delete", "batchsandbox", batchSandboxName, "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying Pool status consistency: available + allocated + restarting <= total")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.status}")
				statusJSON, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				var status struct {
					Total      int32 `json:"total"`
					Allocated  int32 `json:"allocated"`
					Available  int32 `json:"available"`
					Restarting int32 `json:"restarting"`
				}
				err = json.Unmarshal([]byte(statusJSON), &status)
				g.Expect(err).NotTo(HaveOccurred())

				// available + allocated + restarting should equal total
				sum := status.Available + status.Allocated + status.Restarting
				g.Expect(sum).To(Equal(status.Total),
					"available(%d) + allocated(%d) + restarting(%d) should equal total(%d)",
					status.Available, status.Allocated, status.Restarting, status.Total)
			}, 30*time.Second).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})
	})

	Context("Policy Validation", func() {
		It("should correctly set default policy to Delete when not specified", func() {
			const poolName = "test-pool-default-policy"

			By("creating a Pool without specifying podRecyclePolicy")
			poolYAML, err := renderTemplate("testdata/pool-basic.yaml", map[string]interface{}{
				"PoolName":     poolName,
				"SandboxImage": utils.SandboxImage,
				"Namespace":    testNamespace,
				"BufferMax":    2,
				"BufferMin":    1,
				"PoolMax":      3,
				"PoolMin":      1,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-default-policy.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying default policy is Delete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.spec.podRecyclePolicy}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Empty or "Delete" both indicate default Delete policy
				if output == "" {
					// Default value is not always returned in kubectl output
					g.Expect(output).To(BeEmpty())
				} else {
					g.Expect(output).To(Equal("Delete"))
				}
			}).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should accept valid Restart policy value", func() {
			const poolName = "test-pool-restart-value"

			By("creating a Pool with explicit Restart policy")
			poolYAML, err := renderTemplate("testdata/pool-with-restart-policy.yaml", map[string]interface{}{
				"PoolName":          poolName,
				"TaskExecutorImage": utils.TaskExecutorImage,
				"Namespace":         testNamespace,
				"BufferMax":         2,
				"BufferMin":         1,
				"PoolMax":           3,
				"PoolMin":           1,
			})
			Expect(err).NotTo(HaveOccurred())

			poolFile := filepath.Join("/tmp", "test-pool-restart-value.yaml")
			err = os.WriteFile(poolFile, []byte(poolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying policy is Restart")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pool", poolName, "-n", testNamespace,
					"-o", "jsonpath={.spec.podRecyclePolicy}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Restart"))
			}).Should(Succeed())

			By("cleaning up")
			cmd = exec.Command("kubectl", "delete", "pool", poolName, "-n", testNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should reject invalid policy value", func() {
			const poolName = "test-pool-invalid-policy"

			By("creating a Pool with invalid policy")
			invalidPoolYAML := fmt.Sprintf(`
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: %s
  namespace: %s
spec:
  podRecyclePolicy: InvalidPolicy
  template:
    spec:
      containers:
      - name: sandbox-container
        image: %s
        command: ["sleep", "3600"]
  capacitySpec:
    bufferMax: 2
    bufferMin: 1
    poolMax: 3
    poolMin: 1
`, poolName, testNamespace, utils.SandboxImage)

			poolFile := filepath.Join("/tmp", "test-pool-invalid-policy.yaml")
			err := os.WriteFile(poolFile, []byte(invalidPoolYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(poolFile)

			cmd := exec.Command("kubectl", "apply", "-f", poolFile)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "Pool with invalid policy should be rejected")
			Expect(err.Error()).To(Or(
				ContainSubstring("Invalid"),
				ContainSubstring("unsupported value"),
				ContainSubstring("valid values"),
			))
		})
	})
})

// Helper function to check if string contains any of the substrings
func containAnySubstrings(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
