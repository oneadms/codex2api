package auth

import (
	"testing"
)

// Trae 按 x-ide-version-code 灰度下发模型目录：网关必须上报真实的客户端发布码，
// 且保持恒定（不能按当天日期伪造，否则每天变化的版本码本身就是异常指纹）。
func TestTraeCNPreferredIDECodeKeepsRealReleaseCode(t *testing.T) {
	t.Parallel()
	// app 里能读到的 manifest 版本码偏旧（扩展包的版本码），必须回落到真实发布码。
	if got := traeCNPreferredIDECode("20260212"); got != TraeCNDefaultIDECode {
		t.Fatalf("旧 manifest 版本码未被替换: %q", got)
	}
	if got := traeCNPreferredIDECode(""); got != TraeCNDefaultIDECode {
		t.Fatalf("空值未回落到真实发布码: %q", got)
	}
	// 客户端升级后 manifest 给出更新的发布码时采用它。
	if got := traeCNPreferredIDECode("20261015"); got != "20261015" {
		t.Fatalf("更新的发布码未被采用: %q", got)
	}
	// 结果必须稳定：同一天多次调用、不同日期都不会改变。
	if first, second := traeCNPreferredIDECode("20260212"), traeCNPreferredIDECode("20260212"); first != second {
		t.Fatalf("发布码不稳定: %q vs %q", first, second)
	}
}
