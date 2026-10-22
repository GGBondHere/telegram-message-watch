<h1 align="center">📨 Telegram Message Watch</h1>

<p align="center">
  <a href="https://github.com/GGBondHere/telegram-message-watch/releases/latest"><img alt="最新 Release 版本" src="https://img.shields.io/github/v/release/GGBondHere/telegram-message-watch?sort=date&display_name=tag&label=version&color=brightgreen&style=flat-square"></a>
  <a href="#快速开始"><img alt="支持 Debian、Ubuntu、Alpine" src="https://img.shields.io/badge/platform-Debian%20%7C%20Ubuntu%20%7C%20Alpine-0078D4.svg?style=flat-square"></a>
  <img alt="支持 amd64 和 arm64" src="https://img.shields.io/badge/arch-amd64%20%7C%20arm64-blueviolet.svg?style=flat-square">
  <a href="LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/license-MIT-blue.svg?style=flat-square"></a>
</p>

<p align="center"><strong>监听群组与频道，把你关心的 Telegram 消息送到私聊。</strong></p>

<p align="center">
  <a href="#功能亮点">功能亮点</a> ·
  <a href="#快速开始">快速开始</a> ·
  <a href="#设置监听">设置监听</a> ·
  <a href="#更新与维护">更新与维护</a> ·
  <a href="#文件位置">文件位置</a>
</p>

群组消息太多，又不想错过补货、活动或某个人的发言？Telegram Message Watch 使用你的个人账号监听已加入的群组和频道，筛选新消息，再通过通知 Bot 私聊提醒。

**完成首次配置后，日常添加、修改、暂停和删除监听都在 Bot 中操作。**

## 功能亮点

- 🎯 **按需监听**：群组内关键词、指定发送者的全部消息，或指定发送者发出的含关键词消息。
- 🔔 **私聊提醒**：包含来源、发送者、命中原因、摘要及可用的原消息链接。
- 📱 **批量管理**：批量输入，一次确认；首页可快速查看已监听的群组与频道。
- ⏸️ **四级暂停**：全部监听、单个群组、监听项、具体条件均可独立暂停，恢复上一级时保留下级开关。
- 📦 **单文件运行**：支持 amd64 和 arm64，服务器无需安装 Go、Python 或 Docker。

## 快速开始

### 1. 准备账号与服务器

支持 **Debian 11+、Ubuntu 20.04+、Alpine 3.20+** 的 **x86_64（amd64）** 和 **aarch64（arm64）**，使用 root 安装和管理。

| 信息                     | 获取方式                                                              |
| ------------------------ | --------------------------------------------------------------------- |
| `API_ID`、`API_HASH` | 登录 [Telegram API 页面](https://my.telegram.org/apps)，创建应用后获取 |
| Bot Token                | 向 [@BotFather](https://t.me/BotFather) 发送 `/newbot`，创建通知 Bot |
| 个人账号                 | 准备手机号、登录验证码，以及已启用的 2FA 密码                         |

*监听账号需要已加入目标群组或频道，并拥有读取消息的权限。*

### 2. 安装程序

```sh
curl -fsSL https://raw.githubusercontent.com/GGBondHere/telegram-message-watch/main/install.sh | sh
```

> 💡 安装会注册服务和 `tgw`；首次配置完成后才启动监听。重复运行会提示已安装，保留现有程序和数据。

<details>
<summary>离线安装：下载二进制后上传服务器</summary>

从 [Releases](https://github.com/GGBondHere/telegram-message-watch/releases) 下载对应架构的程序，上传到服务器。

| 服务器架构      | 文件                  |
| --------------- | --------------------- |
| x86_64 / amd64  | `tgw-linux-x86_64`  |
| aarch64 / arm64 | `tgw-linux-aarch64` |

两种架构都支持 `install` 参数。以 amd64 为例，在上传目录执行：

```sh
chmod 700 tgw-linux-x86_64
./tgw-linux-x86_64 install
```

arm64 对应执行：

```sh
chmod 700 tgw-linux-aarch64
./tgw-linux-aarch64 install
```

首次离线安装只需对应的程序文件。安装完成后运行 `tgw`，继续下面的首次配置。

已有安装需要更换程序时，从 `tgw → 更新与备份 → 更新程序` 输入程序文件的绝对路径。

</details>

<details>
<summary>尚未安装 curl 或 CA 根证书？</summary>

`curl` 用于下载文件，`ca-certificates` 提供验证 HTTPS 站点所需的 CA 根证书。安装后再执行一键命令。

**Debian / Ubuntu：**

```sh
apt-get update && apt-get install -y curl ca-certificates
```

**Alpine：**

```sh
apk add --no-cache curl ca-certificates
```

</details>

### 3. 完成首次配置

```sh
tgw
```

选择 **1. 首次配置**，依次填写：

**API_ID → API_HASH → Bot Token → 手机号 → 验证码 → 2FA 密码（如有）**。

完成后自动启动监听并设置开机自启。使用刚才登录的个人账号打开通知 Bot，发送 **`/start`**；这个账号也是唯一管理员。

输入时以 `*` 显示敏感内容，支持粘贴和退格。中途退出后可重新选择 `1` 继续；验证码和 2FA 密码不会保存。

## 设置监听

### 选择群组，添加内容

在 Bot 首页点击 **群组 / 频道**，选择目标，再点击 **添加监听**。回复 Bot 的输入提示，核对清单后点击 **确认添加**。

| 监听类型       | 填写示例                         | 提醒范围                         |
| -------------- | -------------------------------- | -------------------------------- |
| 关键词         | `补货, 优惠, 活动`             | 消息包含任意一个关键词           |
| 指定发送者     | `@xiaowang, @xiaoli` 或数字 ID | 任意一个指定发送者的全部消息     |
| 发送者＋关键词 | `@xiaowang, 补货, 优惠`        | 小王发出的消息包含任意一个关键词 |

**组合监听有两种填法**：一次发送“一个发送者＋多个关键词”，或先只发送一个发送者，再按下一条提示填写关键词。逗号、分号、顿号和换行都可以分隔内容；每批最多 30 个关键词，每项最多 80 字符。

群组内的监听项按下面的顺序直接显示在按钮中，没有添加的类型不显示；✅ 表示开启，⏸ 表示暂停。输入用户名后，面板显示解析得到的名称或数字 ID，例如：

```text
✅ 1. 消息包含：补货、优惠、活动
✅ 2. 指定发送者：小王、小李
✅ 3. 小李 的消息包含：优惠、补货
✅ 4. 小王 的消息包含：活动、开奖
```

点击监听项可以继续添加、修改、暂停或删除其中的条件。首页的 **已监听** 方便集中管理；群组列表中 **☐ 表示尚未添加，✅ 表示已添加**，暂停后仍保留勾选。

**各项监听取并集，任意一项命中即可提醒。** 同时开启某人的“全部消息”和关键词组合，仍会提醒这个人的全部消息；组合不会收窄已有范围。添加时会提示范围包含关系，允许继续添加，由你决定是否保留。

**同一条消息跨项命中也只生成一条提醒，命中原因逐行汇总。**

<details>
<summary>通知示例与填写细节</summary>

```text
来源：示例群组 (...)
发送者：小王 (...)

命中：
• 消息包含「补货」
• 小王 的全部消息
• 小王 发出的含「补货」的消息

今天已经补货，晚上八点开始优惠活动。

原消息链接
```

- 关键词匹配正文、媒体说明和消息下方按钮的可见文字，不区分英文大小写，不支持正则表达式。无文字说明的媒体也可通过按钮文字筛选，例如“指定发送者＋一键参与抽奖”；不识别图片内的文字。
- 用户名会解析成固定 ID；也可直接填写数字 ID。详情页显示发送者 ID。
- 重复内容自动跳过。组合中的第一个值始终按发送者处理，其余值按关键词处理。
- 点击现有条件进行修改时，只修改这一条；关键词中的逗号、分号或顿号可在单条修改时填写。
- 长列表和摘要显示省略号，详情页可查看完整关键词；原因过多时显示“另有 N 项命中”。
- 输入流程 10 分钟后过期。切换流程、取消或重启后，需要使用新的输入提示；旧回复和旧按钮不会写入新批次。

</details>

### 暂停与恢复

**首页 → 群组 → 监听项 → 具体条件**，四级都可以单独暂停。暂停上一级不会改变下级开关，恢复后仍按原设置工作。

- **暂停全部监听**：暂停所有群组的匹配和待发通知，保留监听设置。
- **暂停本群组**：暂停这个群组的全部监听，其他群组照常工作。
- **暂停这项监听**：同群组内的其他监听项照常工作，新增条件沿用该项的暂停状态。
- **暂停此条件**：同项内的其他开启条件照常工作。

首页还可点击 **发送验证提醒** 检查私聊是否畅通。命令菜单提供 `/start` 打开首页、`/cancel` 取消输入、`/help` 查看帮助。

## 更新与维护

在服务器运行 `tgw`，按数字选择：

| 菜单                    | 用途                                                              |
| ----------------------- | ----------------------------------------------------------------- |
| **1. 首次配置**   | 填写 API 信息、Bot Token 并登录个人账号；登录失效时也可从这里修复 |
| **2. 修改配置**   | 修改 API 信息、Bot Token，或更换监听账号                          |
| **3. 服务管理**   | 启动、停止、重启服务，或查看实时日志                              |
| **4. 更新与备份** | 更新程序、创建备份、恢复备份、回滚上一份程序和数据                |
| **5. 卸载**       | 询问是否保留数据和备份，默认删除                                  |
| **0. 退出**       | 退出主菜单；子菜单的 `0` 为返回上一级                           |

> 💡 配置修改、更新和恢复会保留原运行状态：原来运行则自动重启，原来停止则保持停止。

常用快捷操作：`tgw start`、`tgw stop`、`tgw restart`、`tgw logs`。

<details>
<summary>更新、备份与回滚的区别</summary>

- **更新程序**：回车选择最新 Release，也可输入版本号或已下载程序的绝对路径。
- **创建备份**：保存当前程序、配置、登录凭据和规则。
- **恢复备份**：按创建时间选择完整备份，恢复对应程序和数据。
- **回滚**：撤销上一次成功的配置修改、更新或恢复；手动创建备份不会改变回滚目标。

配置修改、更新和恢复前会自动备份，保留最近 3 份，并额外保护当前回滚快照。手动备份不会自动删除；在 Bot 修改监听条件不会生成整份程序备份。

更换个人账号后，所有群组停用，条件与各项开关保留，待发通知清除。刷新列表后，可以恢复新账号已加入的同一个群组；其他群组需单独配置。

</details>

版本变化见 [CHANGELOG](CHANGELOG.md)，程序下载见 [Releases](https://github.com/GGBondHere/telegram-message-watch/releases)。

## 文件位置

**监听规则在 Bot 中管理，账号和程序维护通过 `tgw` 操作，无需直接修改文件。**

```text
/opt/tg-watch/
├── current/                # 当前程序入口（软链接）
├── releases/               # 当前和上一版程序
├── data/
│   ├── config.json         # API、Bot 与管理员配置
│   ├── session.json        # Telegram 登录凭据
│   └── state.db            # 监听规则、暂停状态和待发通知
├── backups/                # 手动备份与自动快照
└── logs/                   # Alpine 服务日志
    ├── tg-watch.log
    └── tg-watch.log.1
```

短命令为 `/usr/local/bin/tgw`；Debian / Ubuntu 服务为 `/etc/systemd/system/tg-watch.service`，Alpine 服务为 `/etc/init.d/tg-watch`。

Bot 是操作入口，规则实际保存在服务器的 `data/state.db`。`data/` 和备份包含账号访问凭据，请妥善保管。

## 常见问题

<details>
<summary>为什么收不到提醒？</summary>

确认账号已加入目标群组、该群组已启用、上级没有暂停，且新消息符合至少一个开启的条件。可先发送验证提醒，再用群组内的一条新消息检查匹配。

程序只处理新消息，不补扫历史、不下载媒体。网络超时重试可能产生重复提醒；群组、监听项和条件暂停影响后续匹配，首页暂停也会暂停待发通知，已开始的发送可能完成。

</details>

<details>
<summary>启动失败或登录失效怎么办？</summary>

运行 `tgw logs`，或从服务管理查看日志：先显示最近 80 行，再实时跟随；按 Ctrl+C 返回。日志记录服务状态和脱敏错误，不记录群组消息正文。

Debian / Ubuntu 的 journal 使用系统日志限额；Alpine 的程序日志达到 5 MiB 时轮转，保留当前和上一份。

登录失效时从“首次配置”修复。Bot Token 只能由一个轮询实例使用，冲突时请停止其他实例。

</details>

<details>
<summary>如何迁移备份到另一台服务器？</summary>

1. 在旧服务器创建备份，再停止旧服务。
2. 在新服务器安装程序，将**整个备份目录**放入 `/opt/tg-watch/backups/`。
3. 从“更新与备份 → 恢复备份”选择该备份，恢复后启动服务。

备份目录使用 UTC 时间戳和随机后缀，按内部记录的创建时间排序，改目录名不会改变回滚目标；内部文件及 `meta.json` 请保持原样。

两台服务器的架构和服务管理方式须相同：Debian 与 Ubuntu 之间可以恢复，Alpine 之间可以恢复；不能直接跨 amd64/arm64 或 systemd/OpenRC 恢复。

</details>

## License

项目采用 [MIT License](LICENSE)，第三方组件的版权、许可及源码获取信息见 [第三方声明](THIRD_PARTY_NOTICES.md)，该声明也随 Release 单独提供，转发程序时请同时提供对应声明。
