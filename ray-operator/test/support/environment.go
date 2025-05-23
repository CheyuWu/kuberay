package support

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

const (
	// KuberayTestOutputDir is the testing output directory, to write output files into.
	KuberayTestOutputDir = "KUBERAY_TEST_OUTPUT_DIR"

	// KuberayTestRayVersion is the version of Ray to use for testing.
	KuberayTestRayVersion = "KUBERAY_TEST_RAY_VERSION"

	// KuberayTestRayImage is the Ray image to use for testing.
	KuberayTestRayImage = "KUBERAY_TEST_RAY_IMAGE"

	KuberayTestUpgradeImage = "KUBERAY_TEST_UPGRADE_IMAGE"
)

func GetRayVersion() string {
	return lookupEnvOrDefault(KuberayTestRayVersion, RayVersion)
}

func GetRayImage() string {
	rayImage := lookupEnvOrDefault(KuberayTestRayImage, RayImage)
	// detect if we are running on arm64 machine, most likely apple silicon
	// the os name is not checked as it also possible that it might be linux
	// also check if the image does not have the `-aarch64` suffix
	if runtime.GOARCH == "arm64" && !strings.HasSuffix(rayImage, "-aarch64") {
		rayImage = rayImage + "-aarch64"
		fmt.Printf("Modified Ray Image to: %s for ARM chips\n", rayImage)
	}
	return rayImage
}

func GetKubeRayUpgradeVersion() string {
	return lookupEnvOrDefault(KuberayTestUpgradeImage, KuberayUpgradeVersion)
}

func lookupEnvOrDefault(key, value string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return value
}
