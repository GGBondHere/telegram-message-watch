#!/bin/sh
# Download the native release; installation and service registration stay in Go.
set -eu

REPOSITORY='GGBondHere/telegram-message-watch'
PROJECT='telegram-message-watch'
ROOT='/opt/tg-watch'
SHORTCUT='/usr/local/bin/tgw'
WORK=''
COLOR=0

say() {
    if [ "$COLOR" = 1 ]; then
        printf '\033[%sm%s\033[0m\n' "$1" "$2"
    else
        printf '%s\n' "$2"
    fi
}
fail() { say '1;31' "错误：$*" >&2; exit 1; }
cleanup() { if [ -n "$WORK" ]; then rm -rf -- "$WORK"; fi; }

download() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 2 --connect-timeout 15 --max-time 180 "$1" -o "$2"
    else
        attempt=0
        while [ "$attempt" -lt 3 ]; do
            if wget -q -T 30 -O "$2" "$1"; then return 0; fi
            attempt=$((attempt + 1))
        done
        return 1
    fi
}

check_binary() {
    manifest=$("$1" --manifest) || fail '程序无法运行，请检查架构和文件完整性。'
    for field in '"project":"telegram-message-watch"' '"os":"linux"' "\"arch\":\"$GO_ARCH\""; do
        printf '%s\n' "$manifest" | grep -Fq "$field" ||
            fail '程序的项目标识或目标平台不匹配，未执行安装。'
    done
}

main() {
    case "${1:-}" in
        '') ;;
        -h|--help)
            printf '%s\n' '用法：sh install.sh' \
                '检测系统和架构，安装程序、注册服务与 tgw；不填写 Telegram 配置、不启动服务。' \
                '已安装时保留现有版本。更新请运行 tgw，选择「4. 更新与备份」。'
            return ;;
        *) fail '不支持此参数；使用 sh install.sh --help 查看用法。' ;;
    esac
    [ "$#" -le 1 ] || fail '参数过多。'
    if [ -t 1 ] && [ "${TERM:-dumb}" != dumb ] && [ -z "${NO_COLOR:-}" ]; then COLOR=1; fi
    [ "$(uname -s)" = Linux ] || fail '仅支持 Linux。'
    [ "$(id -u)" = 0 ] || fail '请以 root 身份运行安装引导。'
    [ -r /etc/os-release ] || fail '无法识别系统：缺少 /etc/os-release。'
    . /etc/os-release
    [ -n "${VERSION_ID:-}" ] || fail '无法识别系统版本。'
    major=${VERSION_ID%%.*}
    minor=${VERSION_ID#*.}
    minor=${minor%%.*}
    case "$major" in ''|*[!0-9]*) fail '无法识别系统版本。' ;; esac
    case "${ID:-}" in
        debian) [ "$major" -ge 11 ] || fail '需要 Debian 11 或更高版本。' ;;
        ubuntu)
            case "$minor" in ''|*[!0-9]*) fail '无法识别 Ubuntu 版本。' ;; esac
            [ "$major" -gt 20 ] || { [ "$major" -eq 20 ] && [ "$minor" -ge 4 ]; } ||
                fail '需要 Ubuntu 20.04 或更高版本。' ;;
        alpine)
            case "$minor" in ''|*[!0-9]*) fail '无法识别 Alpine 版本。' ;; esac
            [ "$major" -gt 3 ] || { [ "$major" -eq 3 ] && [ "$minor" -ge 20 ]; } ||
                fail '需要 Alpine 3.20 或更高版本。' ;;
        *) fail '当前支持 Debian 11+、Ubuntu 20.04+、Alpine 3.20+。' ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) ASSET='tgw-linux-x86_64'; GO_ARCH='amd64' ;;
        aarch64|arm64) ASSET='tgw-linux-aarch64'; GO_ARCH='arm64' ;;
        *) fail '仅支持 amd64/x86_64 和 arm64/aarch64。' ;;
    esac
    if [ "$ID" = alpine ]; then
        for tool in rc-service rc-update openrc-run supervise-daemon; do
            command -v "$tool" >/dev/null 2>&1 || fail '需要已安装并启用 OpenRC 的 Alpine 系统。'
        done
        [ -d /run/openrc ] || fail 'OpenRC 尚未启动，请先配置系统的 OpenRC。'
        INIT='OpenRC'
    else
        command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ] ||
            fail '需要正在使用 systemd 的系统。'
        INIT='systemd'
    fi
    say '1;36' "系统：${PRETTY_NAME:-$ID $VERSION_ID}"
    say '1;36' "架构：$GO_ARCH"
    say '1;36' "服务管理：$INIT"

    if [ -e "$ROOT" ] || [ -L "$ROOT" ]; then
        [ -d "$ROOT" ] && [ ! -L "$ROOT" ] || fail '安装路径不是普通目录，拒绝覆盖。'
        if [ -f "$ROOT/.owner" ] && [ ! -L "$ROOT/.owner" ]; then
            [ "$(cat "$ROOT/.owner")" = "$PROJECT" ] || fail '安装目录属于其他程序，拒绝覆盖。'
        else
            [ -z "$(ls -A "$ROOT")" ] || fail '安装目录归属不明，保留原文件。'
        fi
    fi
    if [ -e "$SHORTCUT" ] || [ -L "$SHORTCUT" ]; then
        [ -L "$SHORTCUT" ] && [ "$(readlink "$SHORTCUT")" = "$ROOT/current/tgw" ] ||
            fail 'tgw 命令已被其他程序占用，拒绝覆盖。'
    fi
    if [ -f "$ROOT/installed" ] && [ ! -L "$ROOT/installed" ] && [ -x "$ROOT/current/tgw" ]; then
        [ "$(cat "$ROOT/installed")" = "$PROJECT" ] || fail '安装标记不匹配，保留原文件。'
        check_binary "$ROOT/current/tgw"
        say '1;33' "已安装 Telegram Message Watch $("$ROOT/current/tgw" --version)，无需重复安装。"
        "$ROOT/current/tgw" install </dev/null
        printf '%s\n' '现有程序和数据已保留。请运行 tgw 进行配置或管理。'
        return
    fi

    # Minimal Alpine's BusyBox wget may lack full TLS certificate validation.
    # Use curl with a CA bundle for downloading executable release assets.
    if [ "$ID" = alpine ]; then
        if ! command -v curl >/dev/null 2>&1 ||
            { [ ! -r /etc/ssl/cert.pem ] && [ ! -r /etc/ssl/certs/ca-certificates.crt ]; }; then
            say '1;36' '正在安装 HTTPS 下载工具与 CA 证书……'
            apk add --no-cache curl ca-certificates || fail '下载工具或 CA 证书安装失败。'
        fi
    fi
    command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 ||
        fail '缺少下载工具，请先安装 curl 或 wget。'
    umask 077
    WORK=$(mktemp -d "${TMPDIR:-/tmp}/tgw-install.XXXXXXXX") || fail '无法创建临时目录。'
    trap cleanup 0
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM
    BASE="https://github.com/$REPOSITORY/releases/latest/download"
    say '1;36' "正在下载 $ASSET……"
    download "$BASE/$ASSET" "$WORK/$ASSET" || fail '程序下载失败，请检查网络和 HTTPS 根证书。'
    chmod 700 "$WORK/$ASSET"
    check_binary "$WORK/$ASSET"
    say '1;32' '正在安装并注册服务……'
    "$WORK/$ASSET" install </dev/null
    printf '%s\n' '首次使用时，在面板选择 1. 首次配置。配置完成后才会启动并启用服务。'
}

main "$@"
