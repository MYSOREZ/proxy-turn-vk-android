#!/usr/bin/env bash
# deploy-hy2.sh — ПАРАЛЛЕЛЬНАЯ установка сборки qWDTT-HY2 рядом с уже
# работающим оригинальным qWDTT. Ничего из оригинала не трогает и не удаляет.
#
# Что ставит:
#   1) Hysteria2 (сервер) на локальный порт — он и будет выходной нодой;
#   2) второй экземпляр wdtt-server с флагом -forward, который отдаёт
#      расшифрованный трафик в Hysteria2 вместо встроенного WireGuard.
#
# Транспорт до VPS (VK TURN + DTLS + RTP-обфускация) тот же самый.
# Меняется только то, что едет внутри: вместо WireGuard — QUIC. WireGuard
# как L3-туннель пробрасывает потери релея насквозь, и их разгребает TCP
# внутри туннеля, схлопывая окно; QUIC восстанавливает потери сам.
#
# ─── Карта разделения с оригиналом (ничего не пересекается) ───
#   параметр            оригинал qWDTT      эта сборка HY2
#   DTLS порт           56000               56100
#   внутренний WG порт  56001               56101  (не используется)
#   admin API           56002               56102
#   Hysteria2           —                   56143
#   TUN-интерфейс       wdtt0               wdtthy0
#   каталог конфигов    /etc/wdtt           /etc/wdtt-hy2
#   systemd             wdtt.service        wdtt-hy2.service, hysteria-hy2.service
#   бинарник            wdtt-server         wdtt-hy2-server
#   метка iptables      WDTT_MANAGED        WDTT_HY2_MANAGED
#
# Использование:
#   WDTT_HY2_PASSWORD='<пароль подключения>' bash deploy-hy2.sh
#
# Пароль намеренно один и тот же для qWDTT и для Hysteria2 — приложение
# передаёт его в оба места само, отдельного поля в интерфейсе не нужно.

set -euo pipefail

readonly SCRIPT_VERSION="1.0"
readonly LOG_FILE="/var/log/wdtt-hy2-install.log"

readonly DTLS_PORT="${WDTT_HY2_DTLS_PORT:-56100}"
readonly WG_PORT="${WDTT_HY2_WG_PORT:-56101}"
readonly ADMIN_PORT="${WDTT_HY2_ADMIN_PORT:-56102}"
readonly HY2_PORT="${WDTT_HY2_PORT:-56143}"
readonly SSH_PORT="${WDTT_HY2_SSH_PORT:-22}"

readonly IFACE="wdtthy0"
readonly CONFIG_DIR="/etc/wdtt-hy2"
readonly HY2_CONFIG="/etc/hysteria-hy2/config.yaml"
readonly SERVER_BIN="/usr/local/bin/wdtt-hy2-server"
readonly HY2_BIN="/usr/local/bin/hysteria-hy2"
readonly IPT_COMMENT="WDTT_HY2_MANAGED"
readonly DNS_SERVERS="${WDTT_HY2_DNS:-1.1.1.1,1.0.0.1}"
readonly SNI="${WDTT_HY2_SNI:-bing.com}"

log()  { echo "[$(date '+%H:%M:%S')] $*" | tee -a "$LOG_FILE"; }
die()  { echo "ОШИБКА: $*" | tee -a "$LOG_FILE" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "нужен root"
command -v systemctl >/dev/null 2>&1 || die "нужен VPS с systemd"

PASSWORD="${WDTT_HY2_PASSWORD:-}"
[ -n "$PASSWORD" ] || die "задайте WDTT_HY2_PASSWORD — тот же пароль подключения, что и в приложении"

# ─── Проверка, что не наступаем на оригинал ───
check_port_free() {
    local port="$1" proto="$2"
    if ss -H -lun 2>/dev/null | grep -qE "[:.]${port}\b" && [ "$proto" = udp ]; then
        die "UDP-порт ${port} уже занят. Задайте другой через переменные WDTT_HY2_*"
    fi
}
log "Проверяю, свободны ли порты клона..."
check_port_free "$DTLS_PORT" udp
check_port_free "$HY2_PORT" udp
if [ -d /etc/wdtt ]; then
    log "Обнаружена оригинальная установка /etc/wdtt — она НЕ будет затронута"
fi

mkdir -p "$CONFIG_DIR" "$(dirname "$HY2_CONFIG")"
chmod 700 "$CONFIG_DIR"

# ─── 1. Hysteria2 ───
install_hysteria() {
    if [ -x "$HY2_BIN" ]; then
        log "Hysteria2 уже установлен: $HY2_BIN"
        return
    fi
    log "Ставлю Hysteria2..."
    local arch
    case "$(uname -m)" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        *) die "неподдерживаемая архитектура $(uname -m)" ;;
    esac
    local url="https://github.com/apernet/hysteria/releases/latest/download/hysteria-linux-${arch}"
    curl -fsSL -o "$HY2_BIN" "$url" || die "не скачался Hysteria2 ($url)"
    chmod 755 "$HY2_BIN"
    log "Hysteria2: $("$HY2_BIN" version 2>/dev/null | head -1 || echo установлен)"
}

# ─── 2. Самоподписанный сертификат ───
make_cert() {
    local crt="$(dirname "$HY2_CONFIG")/server.crt"
    local key="$(dirname "$HY2_CONFIG")/server.key"
    if [ -f "$crt" ] && [ -f "$key" ]; then
        log "Сертификат уже есть"
        return
    fi
    log "Генерирую самоподписанный сертификат для SNI=$SNI..."
    openssl req -x509 -nodes -newkey ec:<(openssl ecparam -name prime256v1) \
        -keyout "$key" -out "$crt" -subj "/CN=${SNI}" -days 3650 2>/dev/null \
        || die "не удалось создать сертификат"
    chmod 600 "$key"
}

# ─── 3. Конфиг Hysteria2 ───
write_hy2_config() {
    log "Пишу $HY2_CONFIG (слушает только localhost — наружу он не торчит)..."
    cat > "$HY2_CONFIG" <<HY2CFG
# Слушаем ТОЛЬКО на localhost: снаружи в Hysteria2 попадают исключительно
# пакеты, которые wdtt-hy2-server расшифровал из TURN-туннеля. Наружный
# порт у нас один — DTLS ${DTLS_PORT}.
listen: 127.0.0.1:${HY2_PORT}

tls:
  cert: $(dirname "$HY2_CONFIG")/server.crt
  key: $(dirname "$HY2_CONFIG")/server.key

auth:
  type: password
  password: ${PASSWORD}

# Полосу намеренно не задаём: тогда Hysteria2 использует BBR. Brutal
# включается на стороне клиента флагами -hy2-up/-hy2-down, если BBR окажется
# хуже — так можно сравнить оба варианта, не переустанавливая сервер.
ignoreClientBandwidth: false

masquerade:
  type: proxy
  proxy:
    url: https://${SNI}/
    rewriteHost: true
HY2CFG
    chmod 600 "$HY2_CONFIG"
}

# ─── 4. systemd для Hysteria2 ───
write_hy2_service() {
    cat > /etc/systemd/system/hysteria-hy2.service <<HY2SVC
[Unit]
Description=Hysteria2 (выходная нода для сборки qWDTT-HY2)
After=network.target

[Service]
Type=simple
ExecStart=${HY2_BIN} server -c ${HY2_CONFIG}
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
HY2SVC
    log "hysteria-hy2.service создан"
}

# ─── 5. systemd для второго wdtt-server ───
write_wdtt_service() {
    [ -x "$SERVER_BIN" ] || die "нет $SERVER_BIN — загрузите бинарник сборки HY2 на сервер"
    cat > /etc/systemd/system/wdtt-hy2.service <<WDTTSVC
[Unit]
Description=qWDTT-HY2 (TURN-транспорт -> Hysteria2)
After=network.target hysteria-hy2.service
Wants=hysteria-hy2.service

[Service]
Type=simple
ExecStartPre=-/usr/bin/env bash -c "if command -v iptables >/dev/null 2>&1; then iptables -C INPUT -p udp --dport ${DTLS_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT 2>/dev/null || iptables -I INPUT -p udp --dport ${DTLS_PORT} -m comment --comment ${IPT_COMMENT} -j ACCEPT; iptables -C INPUT ! -i lo -p udp --dport ${HY2_PORT} -m comment --comment ${IPT_COMMENT} -j DROP 2>/dev/null || iptables -I INPUT ! -i lo -p udp --dport ${HY2_PORT} -m comment --comment ${IPT_COMMENT} -j DROP; fi"
ExecStart=${SERVER_BIN} -listen 0.0.0.0:${DTLS_PORT} -forward 127.0.0.1:${HY2_PORT} -wg-port ${WG_PORT} -wg-iface ${IFACE} -config-dir ${CONFIG_DIR} -password-file ${CONFIG_DIR}/main.password -dns ${DNS_SERVERS}
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
WDTTSVC
    log "wdtt-hy2.service создан"
}

main() {
    log "=== Установка qWDTT-HY2 v${SCRIPT_VERSION} (параллельно оригиналу) ==="
    install_hysteria
    make_cert
    write_hy2_config
    printf '%s' "$PASSWORD" > "${CONFIG_DIR}/main.password"
    chmod 600 "${CONFIG_DIR}/main.password"
    write_hy2_service
    write_wdtt_service

    systemctl daemon-reload
    systemctl enable --now hysteria-hy2.service
    systemctl enable --now wdtt-hy2.service
    sleep 2

    echo
    log "=== ГОТОВО ==="
    log "Оригинальный qWDTT не тронут (проверьте: systemctl status wdtt)"
    echo
    echo "  В приложении qWDTT-HY2 укажите:"
    echo "    IP сервера : $(curl -fsS --max-time 5 ifconfig.me 2>/dev/null || echo '<ваш IP>'):${DTLS_PORT}"
    echo "    пароль     : (тот же, что задан в WDTT_HY2_PASSWORD)"
    echo "    режим      : HY2"
    echo
    echo "  Статус:  systemctl status wdtt-hy2 hysteria-hy2"
    echo "  Логи:    journalctl -u wdtt-hy2 -u hysteria-hy2 -f"
    echo "  Удалить: systemctl disable --now wdtt-hy2 hysteria-hy2 && rm -f /etc/systemd/system/{wdtt-hy2,hysteria-hy2}.service && rm -rf ${CONFIG_DIR} $(dirname "$HY2_CONFIG")"
}

main "$@"
