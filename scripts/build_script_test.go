package scripts

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBuildScriptHelpDocumentsReusableInputs(t *testing.T) {
	output, err := exec.Command("./build-linux-amd64.sh", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("build script --help failed: %v\n%s", err, output)
	}

	for _, expected := range []string{
		"Usage:",
		"BUILD_IMAGE",
		"VERSION",
		"dist/cpa-plugin-codex-turn-state-v<version>.so",
	} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("build script help is missing %q:\n%s", expected, output)
		}
	}
}
