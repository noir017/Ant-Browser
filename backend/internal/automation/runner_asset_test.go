package automation

import (
	"regexp"
	"testing"
)

// runner 之间用相对路径 require，漏嵌入的文件只会在真机启动会话时报 MODULE_NOT_FOUND。
func TestRunnerAssetsIncludeRequiredFiles(t *testing.T) {
	pattern := regexp.MustCompile(`require\('\./([^']+\.cjs)'\)`)
	found := 0
	for name, content := range runnerAssetFiles {
		for _, match := range pattern.FindAllSubmatch(content, -1) {
			found++
			if _, ok := runnerAssetFiles[string(match[1])]; !ok {
				t.Errorf("%s requires %s, which is not embedded in runnerAssetFiles", name, match[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("no relative require found; the pattern no longer matches the runner sources")
	}
}
