# ctxmix

[![Go Reference](https://pkg.go.dev/badge/github.com/morsuning/ctxmix.svg)](https://pkg.go.dev/github.com/morsuning/ctxmix)
[![CI](https://github.com/morsuning/ctxmix/actions/workflows/ci.yml/badge.svg)](https://github.com/morsuning/ctxmix/actions/workflows/ci.yml)

**纯 Go、零依赖、追求极限压缩率的压缩库**，基于上下文混合（context mixing），即 PAQ、lpaq、ZPAQ
一脉的技术。在源代码上，输出比 `xz -9e` 小约 **27%**，比 ZPAQ 内置最强模型（`zpaq -m5`）小约
**3.5%**，代价是 CPU 时间是 xz 的数倍。

[English](README.md) · [格式规范](FORMAT.md) · [更新日志](CHANGELOG.md)

```go
import "github.com/morsuning/ctxmix"

// 流式接口，与 compress/flate 类似：
w, _ := ctxmix.NewWriter(dst, ctxmix.WithConcurrency(4))
w.Write(data)
w.Close() // 必须调用

r, _ := ctxmix.NewReader(src)
io.Copy(out, r)

// 或者在内存中：
comp, _ := ctxmix.EncodeAll(data)
back, _ := ctxmix.DecodeAll(comp)
```

命令行工具：

```
go install github.com/morsuning/ctxmix/cmd/ctxmix@latest

ctxmix c -o notes.txt.cmx notes.txt     # 压缩（-level fast、-block MiB、-j N）
ctxmix d -o notes.txt notes.txt.cmx     # 解压（处理不可信数据时加 -max-output 1G）
ctxmix l notes.txt.cmx                  # 查看流的结构
ctxmix t notes.txt.cmx                  # 解压并校验每个块的校验和
```

## 适合你吗？

**体积比时间重要**时用它：发布产物、备份、增量同步包——写一次、走慢速或计费链路传输的数据，
以及源代码和文本归档。

不要用在对延迟或吞吐敏感的路径上，也不要用于已经压缩过的数据（JPEG、MP4、zip 等），也不适合需要
其他工具读取结果的场景：格式是本库自有的。需要标准 ZPAQ 格式时请用
[zpaq-go](https://github.com/morsuning/zpaq-go)，它写出的归档可被官方 `zpaq` 解压，也能读取官方
`zpaq` 的输出。

## 实测数据

单线程，2.1 GHz 云服务器 Xeon（2 vCPU），Go 1.24；ctxmix 使用默认块大小（16 MiB）；`xz -9e -T1`
5.4.5、`bzip2 -9` 1.0.8、`zpaq` 7.15 `-m5 -t1`（单文件归档，含 journaling 开销）。ctxmix 的每个结果
都解压后逐字节比对过。单位：字节。

| 输入 | 大小 | gzip -9 | bzip2 -9 | xz -9e | zpaq -m5 | ctxmix `Fast` | ctxmix `Best` |
|---|---:|---:|---:|---:|---:|---:|---:|
| Go `cmd/compile/internal/ssa`（tar） | 1,505,280 | 369,215 | 290,775 | 286,600 | 216,333 | 226,377 | **208,873** |
| Go `net/…`（tar） | 4,075,520 | 930,426 | 750,811 | 693,600 | 523,382 | 547,218 | **503,885** |
| Go `doc/`（HTML、Markdown；tar） | 20,346,880 | 3,632,156 | 2,297,775 | 895,188 | **667,769** | 784,891 | 723,093 |

相对 `xz -9e`，`Best` 在两个源码输入上小 27.1% 和 27.4%；相对 `zpaq -m5` 小 3.5% 和 3.7%。

第三个输入超过了 16 MiB 的默认块大小，所以 ctxmix 把它压成两个互相独立的块，第二个块没有任何历史
可用——这就是表中它输给 `zpaq -m5` 的原因（大 8.3%）。作为单个块压缩（`-block 32`，即
`WithBlockSize(32 << 20)`）时，`Best` 得到 **629,351** 字节，用时 61 秒：比 `xz -9e` 小 29.7%，比
`zpaq -m5` 小 5.8%。内存允许时请让块大小覆盖你的数据，并且务必在自己的数据上实测。

耗时（同一台机器，单位秒；解压和压缩一样慢，因为解码器运行的是同一个模型）：

| 输入 | xz -9e | zpaq -m5 | ctxmix `Fast` | ctxmix `Best` | `Best` 解压 |
|---|---:|---:|---:|---:|---:|
| ssa.tar | 0.7 | 3.7 | 2.4 | 4.7 | 4.8 |
| net.tar | 2.0 | 8.1 | 6.3 | 12.7 | 12.7 |
| doc/ | 6.3 | 35.9 | 28.1 | 54.6 | 56.8 |

也就是 `Best` 每核约 0.3 到 0.4 MB/s，`Fast` 约为其两倍，输出大约多 8%。`WithConcurrency` 并行处理
互相独立的块（只对超过一个块的输入有帮助）。

复现方法：`ctxmix c -j 1 -o out file`、`xz -9e -T1 -c file | wc -c`、`zpaq a out.zpaq file -m5 -t1`。

## 需要知道的事

* **内存。** 默认块大小下，每个正在处理的块需要约 100 MiB 的模型表（`MemoryUsage` 给出精确值，小块
  需要的少得多），外加数据本身。按 `WithConcurrency` 的工作线程数乘起来预算。
* **块互相独立**，模型在块与块之间会忘掉一切。这使并行成为可能，但对大于一个块的输入会损失压缩率
  （见上面的实测：第三个输入大 15%）。内存允许时用 `WithBlockSize`（最大 64 MiB）。
* **确定性。** 模型只用整数运算，因此输出在所有架构和操作系统上逐位一致。CI 在 amd64、arm64、32 位
  x86 和大端 s390x 上对照冻结的黄金向量检查。
* **不可信输入。** 损坏的数据返回 `ErrCorrupt`，绝不 panic；每个块有 CRC-32，帧结构校验严格。但很小的
  流仍可能描述大量输出，请设置 `WithMaxBlockSize` 和 `WithMaxOutputSize`，并用 `WithContext` 设置
  超时。详见 [SECURITY.md](SECURITY.md)。CRC-32 不是认证手段。
* **稳定性。** 0.x 版本：API 和格式在小版本之间仍可能变化；每次格式变化都会更新版本字节，让旧读取器
  明确报错，并记录在[更新日志](CHANGELOG.md)中。

## API

| | |
|---|---|
| `NewWriter(w, opts...)`、`NewReader(r, opts...)` | 流式；写完要 `Close` 才算结束 |
| `EncodeAll(src, opts...)`、`DecodeAll(comp, opts...)` | 内存中的完整流 |
| `Inspect(r)` | 不解压，直接查看流的结构（级别、块数、大小） |
| `EncodeBlock`、`DecodeBlock` | 无头部的单块，用于自定义容器 |
| `MemoryUsage(blockSize, level)` | 每个工作线程的模型内存 |
| `WithLevel`、`WithBlockSize`、`WithConcurrency` | 压缩设置 |
| `WithMaxBlockSize`、`WithMaxOutputSize`、`WithContext`、`WithProgress` | 限额、取消、进度 |
| `ErrCorrupt`、`ErrTooLarge` | 用 `errors.Is` 判断的错误 |

完整文档见 [pkg.go.dev](https://pkg.go.dev/github.com/morsuning/ctxmix)。

## 原理

每个字节的每一位都由多个模型混合预测，再做算术编码：哈希的 1 到 8 阶上下文、单词和行/缩进相关的上下文
（比特历史经自适应状态映射转为概率）、两个处理长重复的匹配模型、两层门控混合网络，以及两级 APM/SSE
修正。比特历史状态机来自 libzpaq（公有领域）。[FORMAT.md](FORMAT.md) 精确规定了流容器和算术编码器，
并概述了模型。

## 与 zpaq-go 的关系

ctxmix 最初是 [zpaq-go](https://github.com/morsuning/zpaq-go) 的 `ctxmix` 子包，在本仓库的 v0.1.0
中成为独立模块。该子包写出的流可以被原样读取。

## 开发

```
go test ./...                                    # 约一分钟
go test -fuzz FuzzDecode -fuzztime 1m .          # 另有 FuzzDecodeBlock、FuzzRoundtrip
go test -bench . -run '^$' .
```

参见 [CONTRIBUTING.md](CONTRIBUTING.md)；唯一的铁律是压缩后的字节不得意外改变。本模块没有任何依赖。

## 许可证

MIT，见 [LICENSE](LICENSE)。第三方材料与渊源：[NOTICE.md](NOTICE.md)。
