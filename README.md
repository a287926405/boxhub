# BoxHub

**开源自托管的代理控制台 + sing-box 内核管理器,内置链式代理。**

面向 Linux VPS(Debian / Ubuntu / Alpine),一个二进制解决:装内核、管订阅、配置节点组、分流规则,以及**把住宅 / 静态 IP 出口串在自建节点后面**的链式代理。

```
本机 ──▶ 混合端口 :7890 ──▶ 前置节点(你的机场/VPS)──▶ 住宅 IP 出口 ──▶ 互联网
                    │
                    └── 规则命中 → 直连 / 屏蔽 / 指定线路
```

---

## 它解决什么问题

| 场景 | 传统做法 | BoxHub |
|---|---|---|
| 想用住宅 IP 访问某些网站 | 手动改 Clash/sing-box 配置写 `dialer-proxy`,改错就整个内核起不来 | 面板填表建「链式代理」,保存前用内核校验,前端给明确的失败原因 |
| 订阅节点多、想按地区自动分组 | 手写 `proxy-groups` | 动态关键词组,订阅更新自动收编 |
| 换了机器要重配 | 手抄配置 | 导出 / 导入 JSON,自动备份 |
| 配置改坏了打不开 | 重新装 | 保存时校验,不通过就不生效,旧配置继续跑 |

**关键设计:保存 = 校验通过才生效。** 每次修改都会先让内核自己 `check` 一遍,失败则不写入、不重启,你的网络不会因为一次手误中断。

---

## 安装

### 一键安装

Debian / Ubuntu / Alpine(需 root):

```bash
curl -fsSL https://raw.githubusercontent.com/a287926405/boxhub/main/install.sh | bash
```

安装脚本会:

1. 下载对应架构的 sing-box 内核与面板进程(可选下载 zashboard 内核面板)
2. 部署到 `/opt/boxhub`
3. 创建并启动 systemd(或 OpenRC)服务
4. 自动放行防火墙端口
5. 打印访问地址

**首次打开面板需要设置访问密码**,之后每次登录用这个密码。

常用变体:

```bash
# 指定监听端口(默认 3036;想直接用 80 就写 80)
BOXHUB_PORT=80 curl -fsSL https://raw.githubusercontent.com/a287926405/boxhub/main/install.sh | bash

# 国内直连 GitHub 不通:走 gh-proxy 类镜像
BOXHUB_GH_MIRROR=https://gh-proxy.com curl -fsSL https://raw.githubusercontent.com/a287926405/boxhub/main/install.sh | bash

# 已装机器升级(保留配置与现有端口)
curl -fsSL https://raw.githubusercontent.com/a287926405/boxhub/main/install.sh | bash -s -- upgrade

# 装完直接进交互管理菜单(启停/日志/诊断/备份)
curl -fsSL https://raw.githubusercontent.com/a287926405/boxhub/main/install.sh | bash -s -- menu
```

离线 / 内网环境:在能上网的机器下好 [最新 Release](https://github.com/a287926405/boxhub/releases) 里的
`boxhub-x.y.z.tar.gz` 和 sing-box 二进制,拓进目标机后执行:

```bash
tar -xzf boxhub-*.tar.gz && cd boxhub-*
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
sudo BOXHUB_LOCAL_BOXD=bin/boxd-linux-$ARCH BOXHUB_LOCAL_SINGBOX=/path/to/sing-box bash manage.sh install
```

### 指定端口

```bash
sudo BOXHUB_PORT=8080 bash manage.sh install
```

### 管理脚本

```bash
sudo bash manage.sh            # 交互式菜单
sudo bash manage.sh install    # 安装 / 升级
sudo bash manage.sh status     # 查看状态
sudo bash manage.sh logs 200   # 查看日志
bash manage.sh diagnose        # 系统诊断(不需要 root)
sudo bash manage.sh uninstall  # 卸载(可保留配置备份)
```

---

## 快速上手

### 1. 添加节点

「节点与订阅」→ 添加订阅或粘贴分享链接。

支持:`ss` / `vmess` / `vless`(含 REALITY)/ `trojan` / `hysteria2` / `tuic` / `anytls` / `socks` / `http`,
以及 **base64 订阅**、**多行分享链接**、**Clash / Mihomo YAML**。

### 2. 建节点组

「节点组」→ 新建。首次安装会自动创建两个组:

- **手动选择** —— 你日常切线路用的
- **前置线路** —— 专门给链式代理用

组类型:

| 类型 | 说明 |
|---|---|
| 手动选择 | 固定一条线路,不会自动切 |
| 自动择优 | 自动选延时最低 |
| 故障转移 | 按顺序主备 |
| 动态关键词 | 按名称关键词自动收编订阅节点(如填「香港,HK」) |

### 3. 建链式代理

「链式代理」→ 新建:

1. **前置节点组**:选「前置线路」(里面要有节点)
2. **出口**:填住宅/静态 IP 代理的地址与端口,类型选 `socks5` / `http` / `https`

也可以直接粘贴链接:

```
socks5://用户名:密码@1.2.3.4:1080
```

粘贴后自动拆出地址、端口、账号密码。

保存成功后,这条链式代理会作为**普通节点**出现在「手动选择」「自动择优」等组的可选成员里,也可以直接在「目标分流」里作为某条规则的目标。

### 4. 验证链路

在链式代理卡片上点「检测」。它会:

- 经「前置 → 出口」完整链路访问测速地址,得到**端到端延时**
- 访问 IP 回显接口,显示**出口 IP 与归属地**

看到出口 IP 是住宅运营商,就说明链路真的生效了。

---

## 链式代理的实现细节

这是本项目最需要说清楚的部分,因为它是内核里最容易踩坑的地方。

### 它是怎么编到内核配置里的

sing-box 的出站有一个 `detour` 字段,表示"连到这个服务器之前,先经过哪个出站"。链式代理就是利用它:

```json
{
  "outbounds": [
    { "type": "selector", "tag": "group:front", "outbounds": ["node:hk01"], "default": "node:hk01" },
    { "type": "socks",    "tag": "chain:home",  "server": "1.2.3.4", "server_port": 1080,
      "username": "u", "password": "p", "detour": "group:front" }
  ]
}
```

`chain:home` 建立连接时,内核先经 `group:front` 当前选中的节点建立隧道,再从那个隧道连到住宅出口。于是出口看到的源 IP 是前置节点的,而网站看到的是住宅 IP。

### 三条硬性约束(不满足内核直接起不来)

BoxHub 在**保存时**就把这些都拦掉,并给中文原因:

| 约束 | 不满足时内核的报错 |
|---|---|
| 前置组里不能包含这条链式代理自己 | `circular outbound dependency` |
| 两条链式代理不能互为前置 | `circular outbound dependency` |
| 前置组必须有可用节点 | `dependency[group:x] not found` |

第三条尤其容易忽略:**空组不会生成出站**,而链式出站引用它时,`sing-box check` 会通过、`sing-box run` 才报错。BoxHub 在编译阶段和面板上都做了前置拦截。

### 为什么前置必须是一个组

内核要求 `detour` 指向一个具体出站。如果允许"直接选某个节点",那么把链式代理加进这个节点所在的组、又反过来把它当前置,就形成了环。用一个**独立的、不含链式代理的组**作为前置,可以从结构上杜绝这类问题。

### 探测为什么必须走完整链路

只测"出口能不能连"没有意义——住宅代理通常只允许白名单或前置连入。BoxHub 的探测会:

1. 找到包含这条链式代理的可切换组
2. 临时把组切到它
3. 经本地混合端口发请求(内核按选择转发)
4. 拿到延时与出口 IP
5. **无论成败都把组切回原来的选择**

---

## 配置文件

数据目录 `/opt/boxhub/data`:

| 文件 | 说明 |
|---|---|
| `config.json` | 面板配置(权威来源,内核配置由它生成) |
| `config.json.corrupt` | 配置损坏时的留档(自动从备份恢复后生成) |
| `backups/` | 最近 20 次保存的自动备份 |
| `auth.json` | 面板密码(salt + PBKDF2 派生值,不存明文) |
| `kernel.log` | 内核日志 |
| `cache.db` | 内核缓存 |
| `ui/` | metacubexd 静态文件 |

### 导出 / 导入

设置页可以导出整份 JSON 配置。导入时同样会先校验,不通过不会覆盖。

### 配置损坏怎么办

如果 `config.json` 因意外损坏,BoxHub 启动时会:

1. 把坏文件改名为 `config.json.corrupt`
2. 依次尝试最近的备份
3. 恢复成功则正常启动;都失败则用默认配置启动

面板始终能打开——这比"配置坏了就彻底进不去"更重要。

---

## 常用设置

| 设置 | 默认 | 说明 |
|---|---|---|
| 混合代理端口 | 7890 | 同时支持 HTTP 与 SOCKS5 |
| 内核面板端口 | 9090 | metacubexd 用的 Clash API |
| 面板端口 | 3036 | 安装时用 `BOXHUB_PORT` 改 |
| 直连 DNS | 223.5.5.5 | 中国大陆域名用 |
| 代理 DNS | tls://1.1.1.1 | 经代理查询,避免污染 |
| 屏蔽 QUIC | 开 | 很多节点 UDP 转发差,关掉反而更卡 |
| FakeIP | 开 | 走代理的域名返回占位地址 |

「内核面板」入口可以直接打开 metacubexd,看实时连接、按进程/域名排查、手动测速。

---

## 开发

### 目录结构

```
boxhub/
├── cmd/boxd/              主程序
│   ├── main.go
│   └── web/static/        前端(无构建依赖,直接内嵌)
├── internal/
│   ├── config/            配置模型 + 引用校验(链式代理约束在这里)
│   ├── render/            配置编译器(面板配置 → sing-box JSON)
│   ├── store/             持久化(原子写、备份、损坏恢复)
│   ├── kernel/            内核进程管理(校验、热重启、崩溃恢复)
│   ├── sub/               订阅解析(链接 / base64 / Clash YAML)
│   ├── probe/             链路探测(测速 + 出口 IP 归属)
│   └── api/               HTTP 接口 + 认证
├── scripts/
│   ├── manage.sh          安装管理脚本
│   ├── build.sh           构建与打包
│   └── test-manage.sh     脚本逻辑测试
└── dist/                  产物
```

### 构建

```bash
# 需要 Go 1.24+
GO_BIN=/path/to/go bash scripts/build.sh

# 指定版本
VERSION=1.1.0 GO_BIN=/path/to/go bash scripts/build.sh
```

### 测试

```bash
# 全部测试(需要 sing-box 二进制才能跑真实内核测试)
BOXHUB_SING_BOX=/path/to/sing-box go test ./... -v

# 脚本逻辑
bash scripts/test-manage.sh
```

**测试里有几条是真实内核测试**,会用真正的 sing-box 启动生成的配置:

- `TestKernelAcceptsGeneratedConfig` —— 生成的配置能让内核跑起来,且链式代理出现在 Clash API 里
- `TestKernelRunsWithEmptyGroups` —— 空组场景不能出现"check 通过但 run 失败"
- `TestProbeThroughRealChain` —— 真搭一条链,验证探测能拿到出口 IP

这些测试覆盖的都是实际踩过的坑,改动相关代码时不要跳过。

### 踩过的坑(供参考)

1. `sing-box check` **不校验**出站引用是否存在,`route.final` 指向不存在的出站只有 `run` 才报错 → 必须做启动后活性探针
2. `clash_api` 不是入站类型,在 `experimental.clash_api`
3. 1.13 起 legacy inbound 字段(如 `sniff`)被移除,改用 `route.rules` 的 `action`
4. 1.14 起 `dns.fakeip` 顶层字段被移除,fakeip 变成 `dns.servers` 里的一种类型
5. 1.12 起出站必须声明 `domain_resolver`,否则 FATAL
6. HTTP 出站**没有** `network` 字段(写了会 FATAL),SOCKS 才有

---

## 安全

- 面板密码不存明文(PBKDF2-SHA256,12 万次迭代)
- 会话 token 只在 HttpOnly Cookie 里,前端脚本读不到
- 首次设置密码只允许本机/内网 IP 完成,防止公网抢注
- 配置文件权限 0600
- 面板默认监听 `0.0.0.0`:如果机器直接暴露公网,建议在防火墙层限制面板端口来源 IP

---

## 许可

本项目代码采用 MIT 许可。

- **sing-box** 内核由 [SagerNet](https://github.com/SagerNet/sing-box) 提供,遵循其自身许可(GPL-3.0)
- **metacubexd** 面板由 [MetaCubeX](https://github.com/MetaCubeX/metacubexd) 提供,遵循其自身许可

BoxHub 不包含也不修改这两者的源码,仅在运行时下载并调用。
