// 中文说明：验证发行测试版按 SemVer 顺序升级，数字预发布标识不能按字符串排序，构建元数据不参与版本高低。
package update

import "testing"

func TestPrereleaseOrdering(t *testing.T) {
	ordered := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.10", "1.0.0-rc.1", "1.0.0", "1.0.1"}
	for i, left := range ordered {
		for j, right := range ordered {
			if actual := versionLess(left, right); actual != (i < j) {
				t.Fatalf("%s < %s = %v", left, right, actual)
			}
		}
	}
	for _, pair := range [][2]string{{"v1.0.0+build.1", "1.0.0+build.2"}, {"1.0.0-beta.2+build.10", "1.0.0-beta.2+build.1"}} {
		if versionLess(pair[0], pair[1]) || versionLess(pair[1], pair[0]) {
			t.Fatalf("构建元数据影响排序：%v", pair)
		}
	}
}
