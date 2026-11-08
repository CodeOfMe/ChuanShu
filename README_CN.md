# 传书 · chuanshu

**零配置的局域网 P2P 文件传输。** 在存有（超大）文件的机器上运行它，局域网里
的其他人就能*彼此之间*互相下载，而不是全部从你这一台机器拉取。每个下载者都会
自动变成做种者，所以人越多，传得越快。

`传书`（chuanshu）用 BitTorrent 风格的网状网络在局域网内共享文件：文件被切成固
定大小的分块（piece），每个分块都有 SHA-256 校验，节点之间直接交换分块。没有
中心服务器，没有账号，其他机器上除了这一个可执行文件什么都不用装。

```
     ┌──────────┐   share big.iso    ┌──────────┐
     │  主机 A  │◀──────────────────▶│  节点 B  │  从 A 下载分块
     └────┬─────┘   p2p TCP 传分块    └────┬─────┘
          │  ▲                            │  ▼  自己也变成做种者
          │  │ 发现（UDP 组播）           │
     ┌────┴──┴──────┐               ┌─────┴──────┐
     │    节点 C    │◀─────────────▶│   节点 D   │  从 A、B、C… 拉取
     └──────────────┘   p2p TCP      └────────────┘
```

## 特性

- **没有服务器，没有配置。** 节点通过局域网 UDP 组播自动发现彼此，无需 tracker、
  无需端口映射、无需手输 IP。
- **真正的 P2P。** 分块来自所有已持有它的节点，所以下载带宽随参与人数增长，而不
  是卡在原主机身上。
- **完整性有保证。** 每个分块在接受前都会与它的 SHA-256 校验值比对；整个文件由一
  个内容寻址的*信息哈希（info hash）*标识。作恶的节点会被立刻丢弃。
- **可断点续传。** 未完成的下载会保留 `.part` 文件；重新运行命令就只补齐缺少的分块。
- **小巧。** 单个静态可执行文件，仅一个很小的依赖，没有常驻守护进程。

## 安装

`chuanshu` 是一个 Go 单文件可执行程序。

```sh
# 从源码安装（Go 1.26+）
go install github.com/CodeOfMe/ChuanShu/cmd/chuanshu@latest

# 或在仓库内构建
cd chuanshu
go build -o chuanshu ./cmd/chuanshu
```

## 用法

文件由它的**信息哈希**（内容的一个短十六进制摘要）标识。在主机上拿到哈希后，把
哈希发给局域网里任何人即可；也可以让对方用 `ls` 自己发现。

### 1. 共享一个文件（做种）

```sh
chuanshu share bigfile.iso
```

```
Sharing bigfile.iso (4.2 GiB)
  info hash : 3f2a...9c1d
  pieces    : 17203 x 256.0 KiB
  from      : 192.168.1.20:51812

Others on this LAN can download it with:
    chuanshu get 3f2a...9c1d
```

保持它运行，它会向每个来请求的节点做种。按 `Ctrl-C` 停止共享。

### 2. 按信息哈希下载文件

```sh
chuanshu get 3f2a...9c1d --out ./incoming
```

```
Found seeder 192.168.1.20:51812: bigfile.iso (4.2 GiB, 17203 pieces)
Downloading into ./incoming
  42.13%  (7248/17203 pieces, 4 peers, down 1.8 GiB)
  100.00%  (17203/17203 pieces, 6 peers, down 4.2 GiB)

Done: ./incoming/bigfile.iso
You are now seeding it to other peers on this LAN.
```

一个节点一旦下载完成，就会作为做种者对外广播，之后的下载者会从越来越多的来源拉取。

### 3. 浏览局域网里正在共享的文件

```sh
chuanshu ls
```

```
SEEDERS  SIZE      NAME          INFO HASH
2        4.2 GiB   bigfile.iso   3f2a...9c1d
1        180 MiB   demo.mp4      8b0c...77e2
```

### 4. 只计算文件的信息哈希

```sh
chuanshu hash bigfile.iso
```

### 5. 可选：本地网页界面

不想用命令行也可以，直接 `chuanshu serve` 就会启动一个后台节点 + 网页 UI：

```sh
chuanshu serve            # 会打印形如 http://127.0.0.1:52345/ 的 URL 并自动打开浏览器
```

页面上可以用系统原生文件选择框挑本机文件分享(也可直接填路径)、粘贴 info hash 下载、选择接收目录、看进度条。界面内置**四种语言**(中文 / English / 日本語 / Русский),默认按浏览器语言自动匹配,也可在标题栏随时切换。HTTP 服务**只绑定回环地址**（127.0.0.1），局域网里的其他节点仍通过原来的 UDP 组播 + TCP 与节点直接通信，不会暴露给网络。

### 选项

| 命令    | 参数            | 默认  | 含义             |
| ------- | --------------- | ----- | ---------------- |
| `share` | `--piece <KiB>` | `256` | 分块大小（KiB）  |
| `get`   | `--out <dir>`   | `.`   | 输出目录         |
| `ls`    | `--wait <s>`    | `4`   | 监听节点的秒数   |
| `serve` | `--addr <h:p>`  | `127.0.0.1:0` | 网页 UI 绑定的回环地址（端口 0 = 自动） |
| `serve` | `--no-open`     | 关    | 不自动打开浏览器 |

参数可以放在子命令之后的任意位置，例如 `chuanshu get <hash> --out out`，也可写作
`chuanshu get --out out <hash>`。

## 工作原理

- **切块 + 内容寻址。** `share` 把文件切成固定大小的分块，并用 SHA-256 逐块哈希。
  信息哈希是对这些分块哈希（连同文件名、大小、分块大小）再取哈希，于是两个节点
  只凭哈希就能确认“同一个文件”。
- **发现。** 每个节点加入一个 IPv4 组播组，周期性地广播“我（部分）持有哈希 X，
  在 TCP 端口 P”。新加入者在几秒内就能获知所有关心同一文件的节点，全程零配置。
  节点 TCP 端口是临时的，因此同一台机器上可以跑很多实例。
- **传输。** 节点之间通过 TCP 连接，按序号请求分块。提供分块的一方只负责自己位图
  里已验证过的分块，接收一方在落盘前会与期望哈希比对，不一致就丢弃该节点。
- **做种。** “上传者”和“下载者”之间没有区别：任何持有有效分块的节点都会继续对外
  提供，这正是网状网络越传越快的原因。

未完成的下载是一个全长度的 `<name>.part` 文件，分块按各自偏移写入，因此到达顺序
无所谓；续传时会重新校验盘上已有的分块。

## 运行环境

- 构建需要 Go 1.26+（唯一依赖 `golang.org/x/sys`）。
- 本工具使用 IPv4 组播做发现、临时 TCP 端口传数据。某些系统的防火墙首次会弹出授权。
- 发现依赖局域网允许组播。家庭/办公网络一般没问题，但“访客网络”/隔离型 Wi-Fi 或
  系统防火墙常常会拦截组播。
- 已在 linux、darwin、freebsd、openbsd、netbsd、dragonfly、windows（amd64/arm64/386）
  以及 js/wasm、plan9、wasip1 上通过交叉编译验证。Windows 平台使用 IP_ADD_MEMBERSHIP
  但没有 SO_REUSEPORT，所以同一台 Windows 主机上只能启动一个 `chuanshu` 实例来绑定
  共享的发现端口。

## 安全说明

`chuanshu` 对**完整性**是无需信任的（每个分块都校验），但对**隐私**不是：在局域网
里，任何运行本工具的人都能发现正在流转的信息哈希并拉取其任意分块。请不要在你不
信任的网络上共享敏感文件。

## 开发

```sh
go test ./...            # 单元 + 集成测试
go test -race ./...      # 开启数据竞争检测
go vet ./...
gofmt -l .
```

目录结构：

```
cmd/chuanshu/        命令行（share / get / ls / hash）
internal/protocol/   元数据、信息哈希、位图、帧编解码
internal/store/      分块哈希、校验读写、断点续传
internal/discovery/  UDP 组播节点发现与登记表
internal/swarm/      节点：TCP 分块服务、下载循环、做种
internal/web/        可选网页 UI（localhost HTTP 控制面）
```

## 许可

MIT © 2026 cycleuser
