package render_test

// FakeIP 映射持久化的回归测试。
//
// 起因:用户在面板上改「DNS 分流模式」后,所有电脑都上不了网,手机却没事。
//
//   - boxd 每次保存配置都会重启内核(sing-box 没有热重载),Reload = Stop + Start;
//   - 内核的 FakeIP 映射默认只存在内存里,cache_file 没开 store_fakeip 时
//     重启即清空,客户端缓存里 TTL 600 秒的假地址全部失效,
//     日志刷 "missing fakeip record";
//   - 这些目标地址(198.18.0.0/15)又落在私网直连规则里,变成连不通的黑洞;
//   - Windows 的 DNS 缓存 10 分钟 + 长连接常驻,所以电脑一断就是十几分钟,
//     手机锁屏解锁就重新解析,看起来"手机没事"。
//
// 所以:只要开了缓存文件,就**必须**带 store_fakeip,这一条不能回退。

import (
	"strings"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

// 渲染结果必须带 store_fakeip:true。
func TestCacheFileStoresFakeIP(t *testing.T) {
	sb, err := render.Build(config.NewDefault(), render.Context{CacheFile: "/tmp/boxhub-cache.db"})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Experimental == nil || sb.Experimental.CacheFile == nil {
		t.Fatal("没生成 cache_file")
	}
	if !sb.Experimental.CacheFile.StoreFakeIP {
		t.Fatal("cache_file 必须开启 StoreFakeIP,否则内核重启后 FakeIP 映射全丢")
	}
	// 序列化后的字段名必须是 sing-box 认识的 store_fakeip
	raw, _ := sb.Marshal()
	compact := strings.ReplaceAll(string(raw), " ", "")
	if !strings.Contains(compact, `"store_fakeip":true`) {
		t.Fatalf("JSON 里没有字段 store_fakeip:true")
	}
}

// 没配缓存路径时不该凭空冒出 cache_file。
func TestNoCacheFileWhenPathEmpty(t *testing.T) {
	sb, err := render.Build(config.NewDefault(), render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Experimental != nil && sb.Experimental.CacheFile != nil {
		t.Fatal("没有缓存路径时不该生成 cache_file")
	}
}
