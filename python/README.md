# 传书 (chuanshu) - Python

零配置局域网 P2P 文件传输工具的 Python 实现，与 Go 版本 [CodeOfMe/ChuanShu](https://github.com/CodeOfMe/ChuanShu) **完全兼容**:同一条局域网里,Go 与 Python 可以互相分享、互相对端下载。

## 特性

- 零配置:同一局域网内节点自动发现 (IPv4 组播 `239.77.84.1:51218`)
- BitTorrent 式分片:每个分片独立 SHA-256 校验,任何持有完整副本的节点都能变种子
- 边下边做种:下载完成后自动转为种子,越多人下越快
- 本地网页界面:浏览器打开即可拖拽、浏览、下载,原生文件/文件夹选择器
- 中英日俄四语界面,浏览器语言自动检测
- 零第三方依赖:纯标准库
- 与 Go 版本字节级兼容:同文件不同语言 → 同一 info hash

## 安装

```bash
pipx install chuanshu            # 推荐:隔离环境
# 或
python -m pip install chuanshu
```

## 使用

### 打开网页界面(默认)

```bash
chuanshu
```

自动启动一个只监听 `127.0.0.1` 的本地 HTTP 服务,并在默认浏览器中打开。适合双击 / 直接跑。

### 命令行

```bash
chuanshu serve [--addr 127.0.0.1:0] [--no-open]   # 只启动 UI
chuanshu share <file> [--piece KiB]                # 分享文件
chuanshu get   <hash> [--out DIR] [--wait S]       # 通过 info hash 下载
chuanshu ls    [--wait S]                          # 列出局域网上共享的文件
chuanshu hash  <file> [--piece KiB]                # 只计算 info hash
chuanshu version                                   # 版本
```

### 与 Go 版本互通

Go 端:
```bash
chuanshu-go share file.bin
```

Python 端:
```bash
chuanshu get <hash-from-go>
```

反方向亦然。跨语言身份由 SHA-256 校验和 + 分片哈希保证一致。

## 开发

```bash
python -m venv .venv
.venv/bin/pip install -e ".[test]"
.venv/bin/pytest                     # 单元测试 + 集成测试
CHUANSHU_GO=/path/to/chuanshu-go .venv/bin/pytest   # 额外跑 Go 互操作
```

跨语言互操作测试需要本地构建 Go 二进制 (`go build -o chuanshu-go ./cmd/chuanshu`) 并通过 `CHUANSHU_GO` 指定路径,否则会自动跳过。

## 协议兼容性要点

Python 与 Go 在以下所有点字节级一致:

| 位置 | 格式 |
|---|---|
| Info hash | `SHA256(u64be(len(name)) + name + u64be(length) + u64be(pieceLength) + hash0 + hash1 + ...)` |
| 元数据 JSON | `{"name":...,"length":...,"pieceLength":...,"hashes":"hex-of-all-pieces"}` |
| Hello JSON | `{"version":1,"peerId":"base64","infoHash":"base64"}` |
| 帧 | `[1-byte type][4-byte BE length][payload]` |
| 发现包 | magic `0x43485531` `"CHU1"` + 类型 + 数量 + 每条 35 字节 (32 hash + 2 port + 1 flag) |

## 许可

MIT
