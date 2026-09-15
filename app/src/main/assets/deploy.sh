#!/bin/bash
# ==============================================================================
#  WDTT VPN Server — Универсальный установщик для VPS
#  Поддержка: Debian 11+, Ubuntu 20.04+, CentOS/RHEL/Fedora/AlmaLinux/Rocky
#  Версия: 3.2  |  Дата: 2026-05-13
#  NAT:  MASQUERADE через iptables
#  WG:   порт 56101 (сборка HY2; оригинал использует 56001)
#  DTLS: порт 56100 (сборка HY2; оригинал использует 56000)
# ==============================================================================
set -uo pipefail
trap 'rm -f /tmp/wdtt-hy2-admin.token /tmp/wdtt-hy2-main.password /tmp/wdtt-hy2-bot.token' EXIT

readonly SCRIPT_VERSION="3.2"
readonly LOG_FILE="/var/log/wdtt-hy2-install.log"
readonly WG_PORT="${WDTT_WG_PORT:-56101}"
readonly DTLS_PORT="${WDTT_DTLS_PORT:-56100}"
readonly SSH_PORT="${WDTT_SSH_PORT:-22}"
readonly ADMIN_PORT="${WDTT_ADMIN_PORT:-56102}"
# Адаптивная ИИ-маскировка (-ai-listen): отдельный слушатель, потому что её
# формат на проводе не совместим с обычной RTP-обфускацией на -listen.
readonly AI_PORT="${WDTT_HY2_AI_PORT:-56104}"
# Пусто = выключено. Экспериментальный порт для клиентов без DTLS (RTP-obfs AEAD напрямую).
readonly DIRECT_PORT="${WDTT_DIRECT_PORT:-}"
# Raw-IP путь (свой TUN/NAT без WireGuard) в сборке HY2 НЕ используется и
# серверу не передаётся, что бы ни прислало приложение:
#   1. он уходит в сеть мимо -forward, то есть мимо Hysteria2;
#   2. имя интерфейса (wdttraw0), подсеть 10.70.66.0/16, метка iptables
#      WDTT_RAW_MANAGED и таблица nft wdttraw у него захардкожены и общие с
#      оригинальным qWDTT — второй экземпляр снёс бы интерфейс и NAT первого.
readonly RAW_PORT_REQUESTED="${WDTT_RAW_PORT:-}"
readonly RAW_PORT=""
readonly ADMIN_ID="${WDTT_ADMIN_ID:-}"
readonly DNS_SERVERS="${WDTT_DNS_SERVERS:-1.1.1.1,1.0.0.1}"
readonly WDTT_IFACE="wdtthy0"
readonly WDTT_CONFIG_DIR="/etc/wdtt-hy2"
readonly WDTT_ACCESS_DB="passwords.json"
readonly IPT_COMMENT="WDTT_HY2_MANAGED"
# ─── Hysteria2 (сборка HY2) ───
# Выходная нода этой сборки. Слушает ТОЛЬКО localhost: снаружи в неё
# попадает исключительно то, что wdtt-hy2-server расшифровал из TURN-
# туннеля, наружный порт остаётся один — DTLS.
readonly HY2_PORT="${WDTT_HY2_PORT:-56143}"
readonly HY2_BIN="/usr/local/bin/hysteria-hy2"
# Обёртка внешней авторизации Hysteria2 (auth.type: command).
readonly HY2_AUTH_BIN="/usr/local/bin/wdtt-hy2-auth"
readonly HY2_DIR="/etc/hysteria-hy2"
readonly HY2_SNI="${WDTT_HY2_SNI:-bing.com}"
readonly IPT_MIRROR_COMMENT="WDTT_HY2_MIRRORED"

validate_port() {
    local name="$1" value="$2"
    case "$value" in
        ''|*[!0-9]*) die "$name должен быть числом от 1 до 65535, получено: $value" ;;
    esac
    if [ "$value" -lt 1 ] || [ "$value" -gt 65535 ]; then
        die "$name должен быть в диапазоне 1..65535, получено: $value"
    fi
}

validate_admin_id() {
    case "$ADMIN_ID" in
        ''|*[!0-9]*) [ -z "$ADMIN_ID" ] || die "WDTT_ADMIN_ID должен содержать только цифры" ;;
    esac
}

validate_dns_servers() {
    local old_ifs="$IFS" item octet
    IFS=','
    for item in $DNS_SERVERS; do
        case "$item" in
            ''|*[!0-9.]*) IFS="$old_ifs"; die "DNS должен быть IPv4-адресом: $item" ;;
        esac
        IFS='.' read -r o1 o2 o3 o4 extra <<< "$item"
        [ -n "${o1:-}" ] && [ -n "${o2:-}" ] && [ -n "${o3:-}" ] && [ -n "${o4:-}" ] && [ -z "${extra:-}" ] || {
            IFS="$old_ifs"; die "Некорректный DNS IPv4: $item"
        }
        for octet in "$o1" "$o2" "$o3" "$o4"; do
            [ "$octet" -ge 0 ] 2>/dev/null && [ "$octet" -le 255 ] 2>/dev/null || {
                IFS="$old_ifs"; die "Некорректный DNS IPv4: $item"
            }
        done
        IFS=','
    done
    IFS="$old_ifs"
}

# ─── Цвета ───────────────────────────────────────────────────────────────────
C_GREEN=''; C_YELLOW=''; C_RED=''
C_CYAN='';  C_BOLD='';      C_NC=''

log_info()  { echo -e "${C_GREEN}[✓]${C_NC} $*" | tee -a "$LOG_FILE"; }
log_warn()  { echo -e "${C_YELLOW}[!]${C_NC} $*" | tee -a "$LOG_FILE"; }
log_error() { echo -e "${C_RED}[✗]${C_NC} $*" | tee -a "$LOG_FILE"; }
log_step()  { echo -e "${C_CYAN}[►]${C_NC} ${C_BOLD}$*${C_NC}" | tee -a "$LOG_FILE"; }

die() { log_error "$*"; exit 1; }

prog() { echo "WDTT_PROGRESS|$1|$2"; }

# ─── Проверка root ────────────────────────────────────────────────────────────
check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "Скрипт должен быть запущен от root. Если sudo отсутствует, зайдите под root и запустите: bash $0 $*"
    fi
}

# ─── Определение ОС ──────────────────────────────────────────────────────────
OS_ID="" ; PKG_MGR=""

detect_os() {
    log_step "Определение операционной системы..."
    if [ ! -f /etc/os-release ]; then
        die "Файл /etc/os-release не найден."
    fi
    . /etc/os-release
    OS_ID="${ID:-unknown}"
    case "$OS_ID" in
        ubuntu|debian|linuxmint|pop)     PKG_MGR="apt" ;;
        centos|rhel|rocky|almalinux|oracle) PKG_MGR="yum"
            command -v dnf &>/dev/null && PKG_MGR="dnf" ;;
        fedora)                          PKG_MGR="dnf" ;;
        arch|manjaro|endeavouros)        PKG_MGR="pacman" ;;
        *) die "Неподдерживаемый дистрибутив: $OS_ID" ;;
    esac
    log_info "ОС: ${PRETTY_NAME:-$OS_ID} | PM: $PKG_MGR"
}

# ─── Пакеты ──────────────────────────────────────────────────────────────────
pkg_update_done=0

pkg_update() {
    [ "$pkg_update_done" = "1" ] && return 0
    log_step "Обновление индексов пакетов..."
    case "$PKG_MGR" in
        apt)
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -y >>"$LOG_FILE" 2>&1 || log_warn "apt update завершился с ошибкой, пробую продолжить"
            ;;
        dnf)    dnf makecache -y >>"$LOG_FILE" 2>&1 || true ;;
        yum)    yum makecache -y >>"$LOG_FILE" 2>&1 || true ;;
        pacman) pacman -Sy --noconfirm >>"$LOG_FILE" 2>&1 || true ;;
    esac
    pkg_update_done=1
}

pkg_install() {
    [ "$#" -eq 0 ] && return 0
    case "$PKG_MGR" in
        apt)
            export DEBIAN_FRONTEND=noninteractive
            apt-get install -y -qq "$@" >>"$LOG_FILE" 2>&1
            ;;
        dnf)    dnf install -y "$@" >>"$LOG_FILE" 2>&1 ;;
        yum)    yum install -y "$@" >>"$LOG_FILE" 2>&1 ;;
        pacman) pacman -S --noconfirm --needed "$@" >>"$LOG_FILE" 2>&1 ;;
    esac
}

install_prerequisites() {
    prog 0.08 "Пакеты..."
    pkg_update
    log_step "Установка базовых зависимостей..."

    case "$PKG_MGR" in
        apt)
            pkg_install ca-certificates curl openssl iproute2 iptables nftables procps psmisc || \
                log_warn "Часть apt-пакетов не установилась, продолжаю с доступными утилитами"
            ;;
        dnf|yum)
            pkg_install ca-certificates curl openssl iproute iptables nftables procps-ng psmisc || \
                log_warn "Часть rpm-пакетов не установилась, продолжаю с доступными утилитами"
            ;;
        pacman)
            pkg_install ca-certificates curl openssl iproute2 iptables nftables procps-ng psmisc || \
                log_warn "Часть pacman-пакетов не установилась, продолжаю с доступными утилитами"
            ;;
    esac
}

require_runtime_tools() {
    command -v ip >/dev/null 2>&1 || die "Команда ip не найдена. Установите iproute2/iproute."
    command -v systemctl >/dev/null 2>&1 || die "systemctl не найден. Нужен VPS с systemd."
}

# ─── Автоопределение WAN-интерфейса ──────────────────────────────────────────
detect_wan_interface() {
    local iface=""
    iface=$(ip route show default 2>/dev/null | head -1 | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1)}')
    [ -z "$iface" ] && iface=$(ip -4 addr show scope global 2>/dev/null | grep -oP '(?<=dev )\S+' | head -1)
    [ -z "$iface" ] && iface=$(ls /sys/class/net/ | grep -v lo | head -1)
    echo "$iface"
}

# ─── Firewall helpers ────────────────────────────────────────────────────────
FW_BACKEND=""

iptables_add_input() {
    local proto="$1" port="$2" comment="$3"
    [ "$FW_BACKEND" = "iptables" ] || return 0
    case "$proto:$port" in
        tcp:[0-9]*|udp:[0-9]*) ;;
        *) return 0 ;;
    esac
    [ "$port" -ge 1 ] 2>/dev/null && [ "$port" -le 65535 ] 2>/dev/null || return 0
    iptables -C INPUT -p "$proto" --dport "$port" -m comment --comment "$comment" -j ACCEPT 2>/dev/null || \
        iptables -I INPUT -p "$proto" --dport "$port" -m comment --comment "$comment" -j ACCEPT 2>/dev/null || true
}

mirror_port_to_iptables() {
    local proto="$1" port="$2" source="$3"
    iptables_add_input "$proto" "$port" "$IPT_MIRROR_COMMENT"
    log_info "iptables: сохранён доступ $port/$proto из $source"
}

mirror_existing_firewall_ports_to_iptables() {
    [ "$FW_BACKEND" = "iptables" ] || return 0
    local tmp
    tmp="$(mktemp)"

    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi "Status: active"; then
        log_info "UFW активен: переношу разрешённые tcp/udp порты в iptables"
        ufw status 2>/dev/null | sed -nE 's#^([0-9]{1,5})/(tcp|udp)[[:space:]].*ALLOW IN.*#\2 \1 ufw#p' >> "$tmp" || true
    fi

    if command -v nft >/dev/null 2>&1; then
        local nft_ports
        nft_ports="$(nft -a list ruleset 2>/dev/null | sed -nE 's/.*(tcp|udp) dport ([0-9]{1,5}).*accept.*/\1 \2 nft/p' | sort -u || true)"
        if [ -n "$nft_ports" ]; then
            log_info "nftables найден: переношу простые accept dport правила в iptables"
            printf '%s\n' "$nft_ports" >> "$tmp"
        fi
    fi

    if [ -s "$tmp" ]; then
        sort -u "$tmp" | while read -r proto port source; do
            mirror_port_to_iptables "$proto" "$port" "$source"
        done
    else
        log_info "UFW/nftables разрешённых tcp/udp портов для переноса не найдено"
    fi
    rm -f "$tmp"
}

detect_firewall() {
    if ! command -v iptables &>/dev/null; then
        log_warn "iptables не найден. Пытаюсь установить firewall-пакеты..."
        pkg_update
        pkg_install iptables nftables || true
    fi
    if command -v iptables &>/dev/null; then
        FW_BACKEND="iptables"
        log_info "Firewall backend: iptables (принудительно)"
        mirror_existing_firewall_ports_to_iptables
    else
        FW_BACKEND="none"
        log_warn "iptables не найден. Установка продолжится, но NAT/firewall нужно настроить вручную."
    fi
}

# ─── Firewall-абстракция ─────────────────────────────────────────────────────
fw_add_input_udp() {
    local port="$1"
    case "$FW_BACKEND" in
        iptables)
            iptables -C INPUT -p udp --dport "$port" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || \
                iptables -I INPUT -p udp --dport "$port" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            ;;
        nft)
            ensure_nft_wdtt
            nft add rule inet wdtt input udp dport "$port" accept 2>/dev/null || true
            ;;
        none) ;;
    esac
}

fw_restrict_wg_to_loopback() {
    [ "$FW_BACKEND" = "iptables" ] || return 0
    iptables -D INPUT -p udp --dport "$WG_PORT" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
    iptables -C INPUT -i lo -p udp --dport "$WG_PORT" -m comment --comment WDTT_WG_INTERNAL -j ACCEPT 2>/dev/null || \
        iptables -I INPUT -i lo -p udp --dport "$WG_PORT" -m comment --comment WDTT_WG_INTERNAL -j ACCEPT
    iptables -C INPUT ! -i lo -p udp --dport "$WG_PORT" -m comment --comment WDTT_WG_INTERNAL -j DROP 2>/dev/null || \
        iptables -I INPUT ! -i lo -p udp --dport "$WG_PORT" -m comment --comment WDTT_WG_INTERNAL -j DROP
}

fw_add_input_tcp() {
    local port="$1"
    case "$FW_BACKEND" in
        iptables)
            iptables -C INPUT -p tcp --dport "$port" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || \
                iptables -I INPUT -p tcp --dport "$port" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            ;;
        nft)
            ensure_nft_wdtt
            nft add rule inet wdtt input tcp dport "$port" accept 2>/dev/null || true
            ;;
        none) ;;
    esac
}

fw_add_input_udp_range() {
    local from="$1" to="$2"
    case "$FW_BACKEND" in
        iptables|nft) log_warn "Пропускаю широкий UDP range $from-$to: это не изолировано и может влиять на чужие сервисы." ;;
        none) ;;
    esac
}

fw_add_forward() {
    case "$FW_BACKEND" in
        iptables)
            iptables -C FORWARD -i "$WDTT_IFACE" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || \
                iptables -I FORWARD -i "$WDTT_IFACE" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            iptables -C FORWARD -o "$WDTT_IFACE" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || \
                iptables -I FORWARD -o "$WDTT_IFACE" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            ;;
        nft)
            ensure_nft_wdtt
            nft add rule inet wdtt forward iifname "$WDTT_IFACE" accept 2>/dev/null || true
            nft add rule inet wdtt forward oifname "$WDTT_IFACE" accept 2>/dev/null || true
            ;;
        none) ;;
    esac
}

fw_add_masquerade() {
    local iface="$1" subnet="$2"
    case "$FW_BACKEND" in
        iptables)
            iptables -t nat -C POSTROUTING -s "$subnet" -o "$iface" -m comment --comment "$IPT_COMMENT" -j MASQUERADE 2>/dev/null || \
                iptables -t nat -A POSTROUTING -s "$subnet" -o "$iface" -m comment --comment "$IPT_COMMENT" -j MASQUERADE 2>/dev/null || true
            ;;
        nft)
            nft add table ip wdtt 2>/dev/null || true
            nft add chain ip wdtt postrouting '{ type nat hook postrouting priority 100; }' 2>/dev/null || true
            nft add rule ip wdtt postrouting ip saddr "$subnet" oifname "$iface" masquerade 2>/dev/null || true
            ;;
        none) ;;
    esac
}

fw_add_mss_clamping() {
    local subnet="$1"
    case "$FW_BACKEND" in
        iptables)
            # Применяем правило ТОЛЬКО к нашей подсети WDTT
            iptables -t mangle -C FORWARD -s "$subnet" -p tcp -m tcp --tcp-flags SYN,RST SYN -m comment --comment "$IPT_COMMENT" -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null || \
                iptables -t mangle -I FORWARD -s "$subnet" -p tcp -m tcp --tcp-flags SYN,RST SYN -m comment --comment "$IPT_COMMENT" -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null || true
            iptables -t mangle -C FORWARD -d "$subnet" -p tcp -m tcp --tcp-flags SYN,RST SYN -m comment --comment "$IPT_COMMENT" -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null || \
                iptables -t mangle -I FORWARD -d "$subnet" -p tcp -m tcp --tcp-flags SYN,RST SYN -m comment --comment "$IPT_COMMENT" -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null || true
            ;;
        nft)
            nft add table inet wdtt_mangle 2>/dev/null || true
            nft add chain inet wdtt_mangle forward '{ type filter hook forward priority -150; policy accept; }' 2>/dev/null || true
            nft add rule inet wdtt_mangle forward ip saddr "$subnet" tcp flags syn tcp option maxseg size set rt mtu 2>/dev/null || true
            nft add rule inet wdtt_mangle forward ip daddr "$subnet" tcp flags syn tcp option maxseg size set rt mtu 2>/dev/null || true
            ;;
        none) ;;
    esac
}

fw_add_established() {
    return 0
}

fw_cleanup_wdtt_rules() {
    local iface="$1"
    if command -v iptables >/dev/null 2>&1; then
        for i in {1..5}; do
            local nat_iface
            for nat_iface in "$iface" $(ls /sys/class/net 2>/dev/null || true); do
                [ -n "$nat_iface" ] && iptables -t nat -D POSTROUTING -s 10.77.0.0/16 -o "$nat_iface" -m comment --comment "$IPT_COMMENT" -j MASQUERADE 2>/dev/null || true
            done
            iptables -t mangle -D FORWARD -s 10.77.0.0/16 -p tcp -m tcp --tcp-flags SYN,RST SYN -m comment --comment "$IPT_COMMENT" -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null || true
            iptables -t mangle -D FORWARD -d 10.77.0.0/16 -p tcp -m tcp --tcp-flags SYN,RST SYN -m comment --comment "$IPT_COMMENT" -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null || true
            iptables -D INPUT -p udp --dport ${DTLS_PORT} -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            iptables -D INPUT -p tcp --dport ${DTLS_PORT} -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            iptables -D INPUT -p udp --dport ${WG_PORT} -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            iptables -D INPUT -i lo -p udp --dport ${WG_PORT} -m comment --comment WDTT_WG_INTERNAL -j ACCEPT 2>/dev/null || true
            iptables -D INPUT ! -i lo -p udp --dport ${WG_PORT} -m comment --comment WDTT_WG_INTERNAL -j DROP 2>/dev/null || true
            iptables -D INPUT -p tcp --dport ${SSH_PORT} -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            iptables -D INPUT -p tcp --dport 22 -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            iptables -D FORWARD -i "$WDTT_IFACE" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
            iptables -D FORWARD -o "$WDTT_IFACE" -m comment --comment "$IPT_COMMENT" -j ACCEPT 2>/dev/null || true
        done
    fi
    if command -v nft >/dev/null 2>&1; then
        nft delete table ip wdtt 2>/dev/null || true
        nft delete table inet wdtt 2>/dev/null || true
        nft delete table inet wdtt_mangle 2>/dev/null || true
    fi
}

cleanup_config_dir_keep_access_db() {
    [ -d "$WDTT_CONFIG_DIR" ] || return 0
    find "$WDTT_CONFIG_DIR" -mindepth 1 -maxdepth 1 ! -name "$WDTT_ACCESS_DB" -exec rm -rf {} + 2>/dev/null || true
    [ -f "$WDTT_CONFIG_DIR/$WDTT_ACCESS_DB" ] && chmod 600 "$WDTT_CONFIG_DIR/$WDTT_ACCESS_DB" 2>/dev/null || true
}

# ══════════════════════════════════════════════════════════════════════════════
#  WDTT VPN SERVER DEPLOYMENT
# ══════════════════════════════════════════════════════════════════════════════

# ─── Очистка старого WDTT ─────────────────────────────────────────────────────
wdtt_cleanup() {
    prog 0.05 "Очистка..."
    echo "🧹 Очистка старой установки WDTT..."

    systemctl unmask wdtt-hy2 2>/dev/null || true
    systemctl stop wdtt-hy2 2>/dev/null || true
    systemctl disable wdtt-hy2 2>/dev/null || true
    rm -f /etc/systemd/system/wdtt-hy2.service 2>/dev/null || true
    systemctl daemon-reload 2>/dev/null || true
    pkill -x wdtt-hy2-server 2>/dev/null || killall wdtt-hy2-server 2>/dev/null || true

    # Удаляем только собственный интерфейс WDTT.
    ip link show "$WDTT_IFACE" >/dev/null 2>&1 && ip link del "$WDTT_IFACE" 2>/dev/null || true

    # Удаляем старые правила NAT для WDTT подсети
    fw_cleanup_wdtt_rules "$(detect_wan_interface)"

    rm -f /usr/local/bin/wdtt-hy2-server 2>/dev/null || true
    cleanup_config_dir_keep_access_db

    echo "✓ Очистка завершена (база доступа сохранена)"
}

# ─── Sysctl тюнинг ───────────────────────────────────────────────────────────
setup_sysctl() {
    prog 0.20 "Sysctl..."
    echo "⚙️  Настройка сетевых параметров..."

    echo 1 > /proc/sys/net/ipv4/ip_forward 2>/dev/null || true
    mkdir -p /etc/sysctl.d
    cat > /etc/sysctl.d/99-wdtt-hy2.conf << 'SYSEOF'
net.ipv4.ip_forward = 1
SYSEOF

    sysctl -p /etc/sysctl.d/99-wdtt-hy2.conf >/dev/null 2>&1 || true

    echo "✓ Sysctl настроен"
}

# ─── Настройка NAT + Firewall ─────────────────────────────────────────────────
setup_nat_and_firewall() {
    prog 0.40 "NAT + Firewall..."
    echo "🛡  Настройка NAT и фаервола..."

    local iface
    iface=$(detect_wan_interface)

    if [ -z "$iface" ]; then
        log_warn "Не удалось определить WAN-интерфейс!"
        log_warn "Настройте NAT вручную для подсети 10.77.0.0/16."
        return 0
    fi

    log_info "WAN-интерфейс: $iface"

    # === WDTT порты ===
    fw_add_input_udp "$DTLS_PORT"   # DTLS сервер
    fw_add_input_tcp "$DTLS_PORT"   # API (TCP)
    fw_restrict_wg_to_loopback
    fw_add_input_tcp "$ADMIN_PORT"
    fw_add_input_tcp "$SSH_PORT"    # SSH порт, указанный пользователем в приложении
    if [ -n "$DIRECT_PORT" ]; then
        fw_add_input_udp "$DIRECT_PORT"   # -listen-direct: клиенты без DTLS
    fi
    fw_add_input_udp "$AI_PORT"           # -ai-listen: адаптивная ИИ-маскировка
    if [ -n "$RAW_PORT" ]; then
        fw_add_input_udp "$RAW_PORT"   # -listen-raw: raw-IP клиенты без WireGuard
    fi

    # === Forward ===
    fw_add_forward

    # === NAT: MASQUERADE для подсети WireGuard ===
    fw_add_masquerade "$iface" "10.77.0.0/16"
    
    # === MSS Clamping для исправления MTU (DonationAlerts / Cloudflare) ===
    fw_add_mss_clamping "10.77.0.0/16"

    if [ "$FW_BACKEND" = "none" ]; then
        echo "⚠ NAT не настроен автоматически: firewall-бэкенд отсутствует"
    else
        echo "✓ NAT: MASQUERADE на $iface для 10.77.0.0/16"
    fi
    echo "✓ Порты: ${DTLS_PORT}/udp(DTLS), ${AI_PORT}/udp(ИИ-маскировка), ${WG_PORT}/udp(WG), ${SSH_PORT}/tcp(SSH)"
    echo "✓ TCP MSS Clamping включен"
}

# ─── Установка бинарника wdtt-server ──────────────────────────────────────────
setup_wdtt_binary() {
    prog 0.60 "Бинарник..."
    echo "📦 Установка wdtt-server..."

    if [ -f /tmp/wdtt-hy2-server ]; then
        chmod +x /tmp/wdtt-hy2-server
        install -m 0755 /tmp/wdtt-hy2-server /usr/local/bin/wdtt-hy2-server 2>/dev/null || mv /tmp/wdtt-hy2-server /usr/local/bin/wdtt-hy2-server
        echo "✓ wdtt-server установлен"
    elif [ -f /usr/local/bin/wdtt-hy2-server ]; then
        echo "✓ wdtt-server уже установлен"
    else
        echo "⚠ wdtt-server не найден в /tmp/ — пропускаем"
        echo "  Загрузите бинарник вручную в /usr/local/bin/wdtt-hy2-server"
    fi

    mkdir -p "$WDTT_CONFIG_DIR"
}

setup_admin_tls() {
    [ -s /tmp/wdtt-hy2-admin.token ] || die "Токен защищённой админ-панели не загружен"
    cp /tmp/wdtt-hy2-admin.token "$WDTT_CONFIG_DIR/admin.token"
    chmod 0600 "$WDTT_CONFIG_DIR/admin.token"
    rm -f /tmp/wdtt-hy2-admin.token
    if [ ! -s "$WDTT_CONFIG_DIR/admin.crt" ] || [ ! -s "$WDTT_CONFIG_DIR/admin.key" ]; then
        openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 3650 \
            -keyout "$WDTT_CONFIG_DIR/admin.key" \
            -out "$WDTT_CONFIG_DIR/admin.crt" \
            -subj "/CN=qwdtt-admin" >/dev/null 2>&1 || die "Не удалось создать TLS-сертификат"
    fi
    chmod 0600 "$WDTT_CONFIG_DIR/admin.key" "$WDTT_CONFIG_DIR/admin.crt"
    local pin
    pin=$(openssl x509 -in "$WDTT_CONFIG_DIR/admin.crt" -outform DER | openssl dgst -sha256 -binary | openssl base64 -A)
    [ -n "$pin" ] || die "Не удалось получить отпечаток TLS-сертификата"
    echo "WDTT_ADMIN_PIN|sha256/$pin"
}

setup_server_secrets() {
    [ -s /tmp/wdtt-hy2-main.password ] || die "Пароль владельца не загружен"
    install -m 0600 /tmp/wdtt-hy2-main.password "$WDTT_CONFIG_DIR/main.password"
    rm -f /tmp/wdtt-hy2-main.password
    if [ -s /tmp/wdtt-hy2-bot.token ]; then
        install -m 0600 /tmp/wdtt-hy2-bot.token "$WDTT_CONFIG_DIR/bot.token"
    else
        rm -f "$WDTT_CONFIG_DIR/bot.token"
    fi
    rm -f /tmp/wdtt-hy2-bot.token
}

# ─── Systemd-сервис WDTT ─────────────────────────────────────────────────────
setup_wdtt_service() {
    prog 0.75 "Сервис..."
    echo "🔧 Создание systemd-сервиса WDTT..."

    local direct_exec_arg=""
    local direct_fw_rule=""
    if [ -n "$DIRECT_PORT" ]; then
        direct_exec_arg="-listen-direct 0.0.0.0:${DIRECT_PORT}"
        direct_fw_rule="iptables -C INPUT -p udp --dport ${DIRECT_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT 2>/dev/null || iptables -I INPUT -p udp --dport ${DIRECT_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT; "
    fi

    local raw_exec_arg=""
    local raw_fw_rule=""
    if [ -n "$RAW_PORT" ]; then
        raw_exec_arg="-listen-raw 0.0.0.0:${RAW_PORT}"
        raw_fw_rule="iptables -C INPUT -p udp --dport ${RAW_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT 2>/dev/null || iptables -I INPUT -p udp --dport ${RAW_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT; "
    fi

    local bot_exec_arg=""
    if [ -s "$WDTT_CONFIG_DIR/bot.token" ]; then
        bot_exec_arg="-bot-token-file ${WDTT_CONFIG_DIR}/bot.token"
    fi

    local admin_exec_arg=""
    if [ -n "$ADMIN_ID" ]; then
        admin_exec_arg="-admin ${ADMIN_ID}"
    fi

    cat > /etc/systemd/system/wdtt-hy2.service << WDTTSVC
[Unit]
Description=WDTT VPN Server (сборка HY2)
Wants=hysteria-hy2.service
After=hysteria-hy2.service
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=-/usr/bin/env bash -c "ip link show ${WDTT_IFACE} >/dev/null 2>&1 && ip link del ${WDTT_IFACE} 2>/dev/null || true"
ExecStartPre=-/usr/bin/env bash -c "if command -v iptables >/dev/null 2>&1; then iptables -C INPUT -p udp --dport ${DTLS_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT 2>/dev/null || iptables -I INPUT -p udp --dport ${DTLS_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT; iptables -C INPUT -p tcp --dport ${DTLS_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT 2>/dev/null || iptables -I INPUT -p tcp --dport ${DTLS_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT; iptables -C INPUT -i lo -p udp --dport ${WG_PORT} -m comment --comment WDTT_WG_INTERNAL -j ACCEPT 2>/dev/null || iptables -I INPUT -i lo -p udp --dport ${WG_PORT} -m comment --comment WDTT_WG_INTERNAL -j ACCEPT; iptables -C INPUT ! -i lo -p udp --dport ${WG_PORT} -m comment --comment WDTT_WG_INTERNAL -j DROP 2>/dev/null || iptables -I INPUT ! -i lo -p udp --dport ${WG_PORT} -m comment --comment WDTT_WG_INTERNAL -j DROP; iptables -C INPUT -p tcp --dport ${ADMIN_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT 2>/dev/null || iptables -I INPUT -p tcp --dport ${ADMIN_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT; iptables -C INPUT -p tcp --dport ${SSH_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT 2>/dev/null || iptables -I INPUT -p tcp --dport ${SSH_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT; ${direct_fw_rule}${raw_fw_rule}fi"
ExecStart=/usr/local/bin/wdtt-hy2-server -listen 0.0.0.0:${DTLS_PORT} -forward 127.0.0.1:${HY2_PORT} -wg-iface ${WDTT_IFACE} -wg-port ${WG_PORT} -config-dir ${WDTT_CONFIG_DIR} -password-file ${WDTT_CONFIG_DIR}/main.password ${admin_exec_arg} ${bot_exec_arg} -dns ${DNS_SERVERS} -ai-listen 0.0.0.0:${AI_PORT} -admin-listen 0.0.0.0:${ADMIN_PORT} -admin-token-file ${WDTT_CONFIG_DIR}/admin.token -admin-cert ${WDTT_CONFIG_DIR}/admin.crt -admin-key ${WDTT_CONFIG_DIR}/admin.key ${direct_exec_arg} ${raw_exec_arg}
Restart=always
RestartSec=5
LimitNOFILE=65535
UMask=0077

[Install]
WantedBy=multi-user.target
WDTTSVC

    systemctl daemon-reload
    systemctl unmask wdtt-hy2 >/dev/null 2>&1 || true
    systemctl enable wdtt-hy2 >/dev/null 2>&1 || true
    echo "✓ Сервис wdtt-hy2.service создан и включён"
}

# ─── Запуск WDTT ─────────────────────────────────────────────────────────────
start_wdtt() {
    prog 0.90 "Запуск..."
    echo "🚀 Запуск WDTT VPN Server..."

    if [ ! -f /usr/local/bin/wdtt-hy2-server ]; then
        echo "⚠ wdtt-server не установлен — запуск пропущен"
        return 0
    fi

    # Сверяем, что на сервер приехал бинарник ИМЕННО этой сборки. Проверка
    # ТОЛЬКО информационная: запуск с -h в чужом окружении может не дать
    # вывода по причинам, не связанным с бинарником, и раньше такой ложный
    # отказ обрывал установку до настоящей диагностики.
    echo "   Бинарник: $(sha256sum /usr/local/bin/wdtt-hy2-server 2>/dev/null | cut -c1-12), $(stat -c%s /usr/local/bin/wdtt-hy2-server 2>/dev/null) байт"
    local help_out=""
    help_out="$(/usr/local/bin/wdtt-hy2-server -h 2>&1 | head -c 4000)" || true
    if printf '%s' "$help_out" | grep -q -- "-forward"; then
        echo "   Флаг -forward поддерживается: ок"
    else
        echo "⚠ Не удалось подтвердить поддержку -forward (запуск не блокирую)."
        echo "   Код возврата -h: $?, символов вывода: ${#help_out}"
        printf '%s\n' "$help_out" | head -12 | sed 's/^/   >> -h: /'
    fi

    systemctl restart wdtt-hy2

    sleep 2
    local status
    status=$(systemctl is-active wdtt-hy2 2>/dev/null || echo "unknown")

    prog 1.0 "Готово!"

    echo ""
    echo "══════════════════════════════════════════════════════════════"

    if [ "$status" = "active" ]; then
        echo "✅ Деплой успешно завершён!"
        echo "WDTT_DEPLOY_OK"
        echo "   NAT:  MASQUERADE (стандартный)"
        echo "   DTLS: порт ${DTLS_PORT}"
        echo "   WG:   порт ${WG_PORT}"
        echo "   SSH:  порт ${SSH_PORT}"
    else
        echo "⚠️ Сервис wdtt не запустился. Статус: $status"
        systemctl show wdtt-hy2 -p Result -p ExecMainCode -p ExecMainStatus 2>/dev/null \
            | sed 's/^/   >> /'
        echo "   Последние логи сервиса:"
        journalctl -u wdtt-hy2 -n 40 --no-pager -o cat 2>/dev/null | sed 's/^/   >> /'

        # Запускаем ТЕМИ ЖЕ аргументами, что в юните: реконструкция «примерно
        # такой же» командой скрывала бы ошибки в admin API / боте, которых в
        # укороченном наборе флагов просто нет.
        local unit_exec=""
        unit_exec="$(sed -n 's/^ExecStart=//p' /etc/systemd/system/wdtt-hy2.service 2>/dev/null | head -1)"
        if [ -n "$unit_exec" ]; then
            echo "   Прямой запуск с аргументами юнита:"
            echo "   >> $unit_exec"
            timeout 6 bash -c "$unit_exec" 2>&1 | tail -25 | sed 's/^/   >> /'
            echo "   >> (код возврата: $?)"
        fi

        echo "   Занятые порты:"
        (ss -lunp 2>/dev/null || netstat -lunp 2>/dev/null) \
            | grep -E ":(${DTLS_PORT}|${WG_PORT}|${HY2_PORT})[[:space:]]" | sed 's/^/   >> /'
        echo "   Файлы ${WDTT_CONFIG_DIR}:"
        ls -la "${WDTT_CONFIG_DIR}" 2>/dev/null | sed 's/^/   >> /'
        echo "   Состояние Hysteria2: $(systemctl is-active hysteria-hy2 2>/dev/null || echo unknown)"
        journalctl -u hysteria-hy2 -n 10 --no-pager -o cat 2>/dev/null | sed 's/^/   >> HY2: /'
        echo "WDTT_DEPLOY_SERVICE_FAILED"
    fi

    echo "   Логи:   journalctl -u wdtt-hy2 -f"
    echo "   Статус: systemctl status wdtt-hy2"
    echo "══════════════════════════════════════════════════════════════"
    echo ""
}

# ─── Команда: uninstall ──────────────────────────────────────────────────────
do_uninstall() {
    # Сборка HY2: свой Hysteria2 тоже убираем (оригинальной установки не касается)
    systemctl disable --now hysteria-hy2.service 2>/dev/null || true
    rm -f /etc/systemd/system/hysteria-hy2.service 2>/dev/null || true
    rm -rf "$HY2_DIR" 2>/dev/null || true
    rm -f "$HY2_BIN" 2>/dev/null || true
    rm -f "$HY2_AUTH_BIN" 2>/dev/null || true

    log_step "Удаление WDTT..."

    systemctl stop wdtt-hy2 2>/dev/null || true
    systemctl disable wdtt-hy2 2>/dev/null || true
    rm -f /etc/systemd/system/wdtt-hy2.service
    systemctl daemon-reload

    ip link show "$WDTT_IFACE" >/dev/null 2>&1 && ip link del "$WDTT_IFACE" 2>/dev/null || true
    pkill -x wdtt-hy2-server 2>/dev/null || true

    fw_cleanup_wdtt_rules "$(detect_wan_interface)"

    rm -f /usr/local/bin/wdtt-hy2-server
    cleanup_config_dir_keep_access_db
    rm -f /etc/sysctl.d/99-wdtt-hy2.conf
    sysctl --system >/dev/null 2>&1 || true

    log_info "WDTT удалён. База доступа сохранена: ${WDTT_CONFIG_DIR}/${WDTT_ACCESS_DB}"
}

# ─── Команда: status ─────────────────────────────────────────────────────────
do_status() {
    echo "Статус WDTT:"
    echo ""
    if systemctl is-active wdtt-hy2 &>/dev/null; then
        log_info "Сервис: АКТИВЕН"
    else
        log_warn "Сервис: НЕ АКТИВЕН"
    fi
    if [ -f /usr/local/bin/wdtt-hy2-server ]; then
        log_info "Бинарник: установлен"
    else
        log_warn "Бинарник: НЕ найден"
    fi
    if ip link show "$WDTT_IFACE" &>/dev/null; then
        log_info "WDTT интерфейс ($WDTT_IFACE): активен"
    else
        log_warn "WDTT интерфейс ($WDTT_IFACE): не активен"
    fi
}

# ══════════════════════════════════════════════════════════════════════════════
#  MAIN
# ══════════════════════════════════════════════════════════════════════════════
# ─── Установка и настройка Hysteria2 ───────────────────────────────────
install_hysteria2() {
    log_step "Hysteria2"
    mkdir -p "$HY2_DIR"; chmod 700 "$HY2_DIR"

    if [ ! -x "$HY2_BIN" ]; then
        local arch
        case "$(uname -m)" in
            x86_64)  arch="amd64" ;;
            aarch64) arch="arm64" ;;
            *) die "Hysteria2: неподдерживаемая архитектура $(uname -m)" ;;
        esac
        log_info "Скачиваю Hysteria2 (${arch})..."
        local hy2_url="https://github.com/apernet/hysteria/releases/latest/download/hysteria-linux-${arch}"
        if command -v curl >/dev/null 2>&1; then
            curl -fsSL -o "$HY2_BIN" "$hy2_url" || die "Hysteria2 не скачался (curl): $hy2_url"
        elif command -v wget >/dev/null 2>&1; then
            wget -qO "$HY2_BIN" "$hy2_url" || die "Hysteria2 не скачался (wget): $hy2_url"
        else
            die "нет ни curl, ни wget — нечем скачать Hysteria2"
        fi
        [ -s "$HY2_BIN" ] || die "Hysteria2: скачанный файл пуст"
        chmod 755 "$HY2_BIN"
    else
        log_info "Hysteria2 уже установлен"
    fi

    if [ ! -f "$HY2_DIR/server.crt" ] || [ ! -f "$HY2_DIR/server.key" ]; then
        log_info "Генерирую сертификат (CN=${HY2_SNI})..."
        openssl ecparam -name prime256v1 -genkey -noout -out "$HY2_DIR/server.key" 2>/dev/null \
            || die "Hysteria2: не создался ключ"
        openssl req -new -x509 -days 3650 -key "$HY2_DIR/server.key" \
            -out "$HY2_DIR/server.crt" -subj "/CN=${HY2_SNI}" 2>/dev/null \
            || die "Hysteria2: не создался сертификат"
        chmod 600 "$HY2_DIR/server.key"
    fi

    # Пароль Hysteria2 не фиксируем: действующих паролей у qWDTT может быть
    # несколько (владелец из вкладки «Серверы» + пароли устройств из
    # админ-панели/бота), и туннель принимает любой из них. Один пароль в
    # конфиге означал бы, что остальные проходят DTLS, но получают от
    # Hysteria2 редирект маскарада (HTTP 301) — со стороны приложения это
    # выглядит как «authentication error» уже после успешного подключения.
    # Поэтому пароль проверяет сам сервер qWDTT (см. server/authcheck.go).
    [ -s "${WDTT_CONFIG_DIR}/main.password" ] || die "Hysteria2: пустой пароль (${WDTT_CONFIG_DIR}/main.password)"

    cat > "$HY2_AUTH_BIN" <<HY2AUTH
#!/bin/sh
# hysteria зовёт: <addr> <пароль> <tx>; код 0 — пустить, stdout — идентификатор
exec /usr/local/bin/wdtt-hy2-server -config-dir "${WDTT_CONFIG_DIR}" -password-file "${WDTT_CONFIG_DIR}/main.password" -auth-check "\$2"
HY2AUTH
    chmod 755 "$HY2_AUTH_BIN"

    cat > "$HY2_DIR/config.yaml" <<HY2CFG
listen: 127.0.0.1:${HY2_PORT}

tls:
  cert: ${HY2_DIR}/server.crt
  key: ${HY2_DIR}/server.key

auth:
  type: command
  command: ${HY2_AUTH_BIN}

# Полосу намеренно не фиксируем — тогда работает BBR. Brutal включается
# уже на клиенте флагами -hy2-up/-hy2-down, так можно сравнить оба, не
# переустанавливая сервер.
ignoreClientBandwidth: false

masquerade:
  type: proxy
  proxy:
    url: https://${HY2_SNI}/
    rewriteHost: true
HY2CFG
    chmod 600 "$HY2_DIR/config.yaml"

    cat > /etc/systemd/system/hysteria-hy2.service <<HY2SVC
[Unit]
Description=Hysteria2 (выходная нода сборки qWDTT-HY2)
After=network.target

[Service]
Type=simple
ExecStart=${HY2_BIN} server -c ${HY2_DIR}/config.yaml
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
HY2SVC

    systemctl daemon-reload
    systemctl enable hysteria-hy2.service >/dev/null 2>&1 || true
    # Именно restart, а не `enable --now`: на повторной установке сервис уже
    # запущен, и `--now` не перечитал бы новый config.yaml.
    systemctl restart hysteria-hy2.service >/dev/null 2>&1 || true
    sleep 1
    if systemctl is-active --quiet hysteria-hy2.service; then
        log_info "Hysteria2 работает на 127.0.0.1:${HY2_PORT}"
    else
        log_warn "Hysteria2 не поднялся — journalctl -u hysteria-hy2 -n 50"
    fi
}


main() {
    echo "╔══════════════════════════════════════════════════════════════╗"
    echo "║       WDTT VPN Server — Installer v${SCRIPT_VERSION}                    ║"
    echo "║       DTLS: ${DTLS_PORT}  |  WG: ${WG_PORT}  |  SSH: ${SSH_PORT}       ║"
    echo "╚══════════════════════════════════════════════════════════════╝"

    local action="${1:-install}"
    check_root
    validate_port "WDTT_DTLS_PORT" "$DTLS_PORT"
    validate_port "WDTT_WG_PORT" "$WG_PORT"
    validate_port "WDTT_SSH_PORT" "$SSH_PORT"
    validate_port "WDTT_ADMIN_PORT" "$ADMIN_PORT"
    validate_port "WDTT_HY2_PORT" "$HY2_PORT"
    [ -n "$DIRECT_PORT" ] && validate_port "WDTT_DIRECT_PORT" "$DIRECT_PORT"
    if [ -n "$RAW_PORT_REQUESTED" ]; then
        echo "ℹ Raw-IP порт ${RAW_PORT_REQUESTED} проигнорирован: в сборке HY2 этот путь отключён"
    fi
    validate_admin_id
    validate_dns_servers

    mkdir -p "$(dirname "$LOG_FILE")"
    echo "=== WDTT Installer v${SCRIPT_VERSION} — $(date) ===" >> "$LOG_FILE"

    detect_os
    install_prerequisites
    require_runtime_tools
    detect_firewall

    case "$action" in
        status|--status|-s)       do_status ;;
        uninstall|--uninstall|-u) do_uninstall ;;
        install|--install|-i|*)
            wdtt_cleanup
            setup_sysctl
            setup_nat_and_firewall
            setup_wdtt_binary
            setup_admin_tls
            setup_server_secrets
            install_hysteria2
            setup_wdtt_service
            start_wdtt
            ;;
    esac
}

main "$@"
