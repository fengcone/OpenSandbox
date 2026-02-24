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

package kata

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/test/e2e_runtime"
)

// runKubectl executes a kubectl command from the project root directory
func runKubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Dir = "../../.." // 从 test/e2e_runtime/kata 回到项目根目录
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("kubectl %v failed: %w", args, err)
	}
	return string(output), nil
}

var _ = Describe("Kata Containers RuntimeClass", Ordered, func() {
	const testNamespace = "default"

	BeforeAll(func() {
		By("installing Kata Containers RuntimeClass")
		_, err := runKubectl("apply", "-f", "test/e2e_runtime/kata/testdata/runtimeclass.yaml")
		Expect(err).NotTo(HaveOccurred(), "Failed to create Kata RuntimeClass")
	})

	AfterAll(func() {
		By("cleaning up RuntimeClass")
		_, _ = runKubectl("delete", "runtimeclass", RuntimeClassName, "--ignore-not-found=true")
	})

	Context("RuntimeClass API", func() {
		It("should create RuntimeClass resources", func() {
			By("verifying RuntimeClass exists")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "runtimeclass", RuntimeClassName, "-o", "json")
				g.Expect(err).NotTo(HaveOccurred())

				var rcObj struct {
					Handler string `json:"handler"`
				}
				err = json.Unmarshal([]byte(output), &rcObj)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rcObj.Handler).To(Equal("kata-qemu"))
			}, 30*time.Second).Should(Succeed())
		})
	})

	Context("Pod with runtimeClassName", func() {
		var podName string

		BeforeEach(func() {
			podName = fmt.Sprintf("test-pod-kata-%d", time.Now().UnixNano())
		})

		AfterEach(func() {
			By("cleaning up Pod")
			if podName != "" {
				_, _ = runKubectl("delete", "pod", podName, "-n", testNamespace, "--ignore-not-found=true")
			}
		})

		It("should create Pod with runtimeClassName", func() {
			By("creating a Pod with runtimeClassName")
			podYAML := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  runtimeClassName: %s
  containers:
  - name: test-container
    image: %s
    command: ["sleep", "3600"]
`, podName, testNamespace, RuntimeClassName, e2e_runtime.SandboxImage)

			podFile := filepath.Join("/tmp", fmt.Sprintf("test-pod-%s.yaml", podName))
			err := os.WriteFile(podFile, []byte(podYAML), 0644)
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(podFile)

			_, err = runKubectl("apply", "-f", podFile)
			Expect(err).NotTo(HaveOccurred(), "Failed to create Pod")

			By("verifying Pod has runtimeClassName set")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.spec.runtimeClassName}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(RuntimeClassName))
			}, 30*time.Second).Should(Succeed())

			By("verifying Pod is running with Kata")
			Eventually(func(g Gomega) {
				output, err := runKubectl("get", "pod", podName, "-n", testNamespace,
					"-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}, 3*time.Minute).Should(Succeed()) // Kata may take longer to start
		})
	})
})
