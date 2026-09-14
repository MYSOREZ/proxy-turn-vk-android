package main

// wgIfaceName — имя TUN-интерфейса встроенного WireGuard. Намеренно не
// константа: на одном VPS может работать несколько экземпляров сервера
// (напр. оригинальный qWDTT и параллельная сборка HY2), а при старте сервер
// делает `ip link del <iface>` — с общим именем второй экземпляр снёс бы
// интерфейс первого. Переопределяется флагом -wg-iface.
var wgIfaceName = "wdtt0"

const (
	wgServerAddr          = "10.66.66.1"
	wgServerCIDR          = wgServerAddr + "/16"
	defaultInternalWGPort = 56001
	wgMTU                 = 1280
	keepalive             = 25

	// Raw-IP роутер (без WireGuard) — отдельный TUN/подсеть/NAT, полностью
	// параллельно WG-пути. Подсеть намеренно не пересекается с wgServerCIDR.
	rawIfaceName  = "wdttraw0"
	rawServerAddr = "10.70.66.1"
	rawServerCIDR = rawServerAddr + "/16"
	// Raw-режим не несёт WG data header (~32 байта) — только RTP-obfs (12 байт
	// заголовок + 16 байт AEAD tag + до 60 байт padding в video-режиме) и TURN
	// ChannelData/Send Indication framing (4-24 байта). Даже в худшем случае
	// (video-режим, максимальный padding) итоговый размер на проводе — около
	// 1420 байт с MTU=1300, что укладывается в стандартный Ethernet MTU 1500
	// без фрагментации.
	rawMTU = 1300
)

var dns = "8.8.8.8"
