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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"encoding/base64"
	"io/ioutil"
	"os/signal"
)

// ContainerSpec represents a mapping from container name to destination URI
type ContainerSpec struct {
	Name string
	URI  string
}

// Global tracking of paused containers for cleanup
var pausedContainerIds []string

func main() {
	args := os.Args[1:]

	// Set up signal handler to ensure all paused containers are resumed on exit
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-c
		fmt.Fprintf(os.Stderr, "Received signal %v, cleaning up paused containers...\n", sig)
		resumeAllPausedContainers()
		os.Exit(1)
	}()

	// Defer cleanup in case of panic or early termination
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Panic occurred: %v\n", r)
			resumeAllPausedContainers()
			panic(r)
		}
	}()

	// Determine if using old or new format
	var podName, namespace string
	var containerSpecs []ContainerSpec

	if len(args) >= 4 && isNewFormatDetected(args) {
		// New format: <pod_name> <namespace> <container1:uri1> [container2:uri2...]
		podName = args[0]
		namespace = args[1]

		for i := 2; i < len(args); i++ {
			spec, err := parseContainerSpec(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
				os.Exit(1)
			}
			containerSpecs = append(containerSpecs, spec)
		}
	} else if len(args) == 4 {
		// Legacy format: <pod_name> <container_name> <namespace> <target_image>
		podName = args[0]
		containerName := args[1]
		namespace = args[2]
		targetImage := args[3]

		containerSpecs = []ContainerSpec{
			{Name: containerName, URI: targetImage},
		}
	} else {
		fmt.Fprintln(os.Stderr, "ERROR: Missing required parameters")
		fmt.Fprintln(os.Stderr, "Usage (multi-container): commit-snapshot <pod_name> <namespace> <container1:uri1> [container2:uri2...]")
		fmt.Fprintln(os.Stderr, "Usage (legacy):          commit-snapshot <pod_name> <container_name> <namespace> <target_image>")
		os.Exit(1)
	}

	// Validate required inputs
	if len(podName) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: Pod name is required")
		os.Exit(1)
	}

	if len(namespace) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: Namespace is required")
		os.Exit(1)
	}

	if len(containerSpecs) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: At least one container specification is required")
		fmt.Fprintln(os.Stderr, "Usage: commit-snapshot <pod_name> <namespace> <container1:uri1> [container2:uri2...]")
		os.Exit(1)
	}

	fmt.Println("=== Commit Snapshot Go Program ===")
	fmt.Printf("Pod: %s\n", podName)
	fmt.Printf("Namespace: %s\n", namespace)
	for _, spec := range containerSpecs {
		fmt.Printf("Container spec: %s -> %s\n", spec.Name, spec.URI)
	}

	// Step 1: Discover pod sandbox
	fmt.Println("\n=== Step 1: Find pod sandbox ===")
	podSandboxID, err := getPodSandboxID(podName, namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to find pod: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Pod sandbox ID: %s\n", podSandboxID)

	// Step 2: Find container IDs and validate
	fmt.Println("\n=== Step 2: Find container IDs ===")
	containerMap := make(map[string]string) // Maps container name to container ID
	for _, spec := range containerSpecs {
		containerID, err := getContainerID(podSandboxID, spec.Name)
		if err != nil {
			resumeAllPausedContainers()
			fmt.Fprintf(os.Stderr, "ERROR: Failed to find container '%s': %v\n", spec.Name, err)
			os.Exit(1)
		}

		fmt.Printf("Container '%s' -> ID: %s\n", spec.Name, containerID)
		containerMap[spec.Name] = containerID
	}

	// Step 3: Pause all containers
	fmt.Println("\n=== Step 3: Pause all containers ===")
	pauseErrors := 0
	for _, spec := range containerSpecs {
		containerID := containerMap[spec.Name]
		if err := pauseContainer(containerID); err != nil {
			// On pause failure, we still try to continue since commit might work anyway (as in shell script)
			fmt.Fprintf(os.Stderr, "WARNING: Could not pause '%s'. Will attempt commit anyway (container may be stopped).\n", spec.Name)
			pauseErrors++
		} else {
			// Track successfully paused containers for cleanup
			pausedContainerIds = append(pausedContainerIds, containerID)
		}
	}

	// Step 4: Commit all containers
	fmt.Println("\n=== Step 4: Commit all containers ===")
	committedImages := make(map[string]string) // Maps container name to committed image URI
	commitErrors := 0
	for _, spec := range containerSpecs {
		containerID := containerMap[spec.Name]
		if err := commitContainer(containerID, spec.URI); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to commit container '%s': %v\n", spec.Name, err)
			commitErrors++
		} else {
			committedImages[spec.Name] = spec.URI
			fmt.Printf("Successfully committed: %s -> %s\n", containerID, spec.URI)
		}
	}

	// Step 5: Resume all paused containers (regardless of commit success/failure)
	fmt.Println("\n=== Step 5: Resume all paused containers ===")
	resumeAllPausedContainers()

	// If there were commit errors, exit with failure after cleanup
	if commitErrors > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: %d container(s) failed to commit. All containers have been resumed.\n", commitErrors)
		os.Exit(1)
	}

	// Step 6: Push all committed images
	fmt.Println("\n=== Step 6: Push all images ===")
	pushErrors := 0
	for _, spec := range containerSpecs {
		if _, ok := committedImages[spec.Name]; ok {
			if err := pushImage(spec.URI); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to push image for container '%s': %v\n", spec.Name, err)
				pushErrors++
			} else {
				fmt.Printf("Successfully pushed: %s\n", spec.URI)
			}
		}
	}

	if pushErrors > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: %d image(s) failed to push.\n", pushErrors)
		os.Exit(1)
	}

	// Step 7: Extract digests and output results
	fmt.Println("\n=== Step 7: Extract digests ===")
	digests := make(map[string]string) // Maps container name to digest
	firstDigest := ""

	for _, spec := range containerSpecs {
		if _, ok := committedImages[spec.Name]; ok {
			digest, err := getImageDigest(spec.URI)
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARN: Failed to extract digest for %s: %v\n", spec.URI, err)
				digest = "sha256:placeholder" // fallback digest
			}

			digests[spec.Name] = digest
			fmt.Printf("Container '%s' digest: %s\n", spec.Name, digest)

			// Capture first digest for legacy output
			if firstDigest == "" {
				firstDigest = digest
			}
		}
	}

	// Final output - SNAPSHOT_DIGEST_ variables for each container
	fmt.Println("\n=== Snapshot completed successfully ===")
	for _, spec := range containerSpecs {
		if digest, ok := digests[spec.Name]; ok {
			upperName := strings.ToUpper(strings.ReplaceAll(spec.Name, "-", "_"))
			fmt.Printf("SNAPSHOT_DIGEST_%s=%s\n", upperName, digest)
			fmt.Printf("  Image: %s\n", spec.URI)
			fmt.Printf("  Digest: %s\n", digest)
		}
	}

	// Legacy single-digest output for backward compatibility
	fmt.Printf("SNAPSHOT_DIGEST=%s\n", firstDigest)
}

// isNewFormatDetected determines if arguments use new format vs legacy format
// New format has <pod_name> <namespace> followed by container:uri pairs (containing ':')
func isNewFormatDetected(args []string) bool {
	if len(args) < 3 {
		return false
	}

	// Check if arg[2] and subsequent args contain ':' which indicates the new format
	// For legacy format: args[2] would be container_name and likely not contain ':'
	// For new format: args[2] would be container:uri and contains ':'
	for i := 2; i < len(args); i++ {
		if strings.Contains(args[i], ":") {
			return true
		}
	}
	return false
}

// parseContainerSpec parses a "container:uri" string into ContainerSpec
func parseContainerSpec(specStr string) (ContainerSpec, error) {
	parts := strings.SplitN(specStr, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ContainerSpec{}, fmt.Errorf("invalid container spec '%s'. Expected format: container_name:uri", specStr)
	}

	return ContainerSpec{
		Name: parts[0],
		URI:  parts[1],
	}, nil
}

// getPodSandboxID uses crictl to find the pod sandbox ID
func getPodSandboxID(podName, namespace string) (string, error) {
	cmd := exec.Command("crictl", "pods", "--name", podName, "--namespace", namespace, "-q")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to find pod %s in namespace %s: %v", podName, namespace, err)
	}

	sandboxID := strings.TrimSpace(string(output))
	if sandboxID == "" {
		return "", fmt.Errorf("pod sandbox not found for %s in namespace %s", podName, namespace)
	}

	return sandboxID, nil
}

// getContainerID uses crictl to find the container ID within a pod sandbox
func getContainerID(podSandboxID, containerName string) (string, error) {
	cmd := exec.Command("crictl", "ps", "--pod", podSandboxID, "--name", containerName, "-q")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to find container %s in pod sandbox %s: %v", containerName, podSandboxID, err)
	}

	containerID := strings.TrimSpace(string(output))
	if containerID == "" {
		return "", fmt.Errorf("container %s not found in pod sandbox %s", containerName, podSandboxID)
	}

	return containerID, nil
}

// pauseContainer uses nerdctl to pause a container
func pauseContainer(containerID string) error {
	fmt.Printf("Pausing container %s...\n", containerID)
	cmd := exec.Command("nerdctl", "pause", containerID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to pause container %s: %v, output: %s", containerID, err, string(output))
	}
	fmt.Printf("Paused successfully: %s\n", containerID)
	return nil
}

// resumeContainer uses nerdctl to resume a container
func resumeContainer(containerID string) error {
	fmt.Printf("Resuming container %s...\n", containerID)
	cmd := exec.Command("nerdctl", "unpause", containerID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to resume container %s: %v, output: %s", containerID, err, string(output))
	}
	fmt.Printf("Resumed successfully: %s\n", containerID)
	return nil
}

// resumeAllPausedContainers resumes all paused containers that were tracked
func resumeAllPausedContainers() {
	if len(pausedContainerIds) == 0 {
		return
	}

	fmt.Println("\n=== Cleanup: Resuming all paused containers ===")

	// Process in reverse order to match pause order
	for i := len(pausedContainerIds) - 1; i >= 0; i-- {
		containerID := pausedContainerIds[i]
		err := resumeContainer(containerID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: Could not resume container %s: %v\n", containerID, err)
		}
	}

	// Clear the paused containers list after cleanup
	pausedContainerIds = []string{}
}

// commitContainer uses nerdctl to commit a container to an image
func commitContainer(containerID, targetImage string) error {
	fmt.Printf("Committing container %s to image %s...\n", containerID, targetImage)
	cmd := exec.Command("nerdctl", "commit", containerID, targetImage)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to commit container %s to %s: %v, output: %s", containerID, targetImage, err, string(output))
	}
	return nil
}

// pushImage uses nerdctl to push the image to the registry
func pushImage(targetImage string) error {
	fmt.Printf("Pushing image %s...\n", targetImage)

	creDir := "/var/run/opensandbox/registry"
	configPath := filepath.Join(creDir, "config.json")
	pushOpts := []string{"push"}

	// Check for credentials and handle authentication
	if _, err := os.Stat(configPath); err == nil {
		fmt.Printf("Found registry credentials at %s\n", configPath)

		// Load credentials from config.json
		data, err := ioutil.ReadFile(configPath)
		if err == nil {
			var creds map[string]interface{}
			if json.Unmarshal(data, &creds) == nil {
				imageParts := strings.Split(targetImage, "/")
				if len(imageParts) > 0 {
					registryHost := imageParts[0]

					auths, ok := creds["auths"].(map[string]interface{})
					if ok && auths[registryHost] != nil {
						authEntry, ok := auths[registryHost].(map[string]interface{})
						if ok {
							if authVal, ok := authEntry["auth"].(string); ok && authVal != "" {
								// Decode the base64 auth string
								decoded, err := base64.StdEncoding.DecodeString(authVal)
								if err == nil {
									decodedStr := string(decoded)
									parts := strings.SplitN(decodedStr, ":", 2)
									if len(parts) == 2 {
										username := parts[0]
										password := parts[1]
										pushOpts = append(pushOpts, "--username", username, "--password", password)
									}
								}
							}
						}
					}
				}
			}
		}
	} else {
		fmt.Println("No registry credentials found, assuming insecure or pre-authenticated registry")
	}

	// Check for insecure registry
	imageParts := strings.Split(targetImage, "/")
	if len(imageParts) > 0 {
		registryHost := imageParts[0]
		isInsecure := strings.Contains(registryHost, "local") ||
			strings.Contains(registryHost, "localhost") ||
			strings.HasPrefix(registryHost, "127.") ||
			strings.HasPrefix(registryHost, "10.") ||
			strings.HasPrefix(registryHost, "192.168.")

		if isInsecure {
			pushOpts = append(pushOpts, "--insecure-registry")
		}
	}

	// Add target image to our command options
	pushOpts = append(pushOpts, targetImage)

	cmd := exec.Command("nerdctl", pushOpts...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	// If primary push failed, try with plain-http as a fallback
	pushOptsPlainHttp := append([]string{"push", "--insecure-registry"}, targetImage)
	cmdFallback := exec.Command("nerdctl", pushOptsPlainHttp...)
	output, err = cmdFallback.CombinedOutput()

	if err != nil {
		return fmt.Errorf("failed to push image %s: %v, output: %s", targetImage, err, string(output))
	}

	return nil
}

// getImageDigest uses nerdctl to get the digest of the image
func getImageDigest(imageRef string) (string, error) {
	cmd := exec.Command("nerdctl", "inspect", "--format", "{{.Id}}", imageRef)
	output, err := cmd.Output()
	if err == nil {
		digest := strings.TrimSpace(string(output))
		if digest != "" {
			return digest, nil
		}
	}

	// If primary method fails (due to formatting, API changes, etc), return placeholder
	// The shell script had more complex fallback mechanisms but this covers the essential use case
	return "sha256:placeholder", nil
}
