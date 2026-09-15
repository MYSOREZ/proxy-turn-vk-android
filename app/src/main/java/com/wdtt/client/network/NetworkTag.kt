package com.wdtt.client

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.telephony.TelephonyManager

/**
 * Метка текущей сети для памяти адаптивной маскировки.
 *
 * Зачем отдельная метка: фильтрация у домашнего Wi-Fi и у соты оператора
 * разная, и то, что выучено в одной сети, в другой скорее вредит. Память
 * ядра раскладывается по этой метке (см. go_client/aistate.go).
 *
 * Что НЕ входит в метку: SSID (требует разрешения на геолокацию и указывает
 * на место), идентификаторы SIM, IP. Только тип транспорта и имя оператора —
 * этого достаточно, чтобы отличить «домашний Wi-Fi» от «сота Beeline», и
 * недостаточно, чтобы кого-то опознать.
 */
object NetworkTag {
    fun current(context: Context): String {
        val cm = context.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
            ?: return "unknown"
        val caps = runCatching { cm.getNetworkCapabilities(cm.activeNetwork) }.getOrNull()
            ?: return "offline"

        return when {
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "wifi"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "eth"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> cellularTag(context)
            else -> "other"
        }
    }

    private fun cellularTag(context: Context): String {
        val tm = context.getSystemService(Context.TELEPHONY_SERVICE) as? TelephonyManager
        // networkOperatorName не требует разрешений; на некоторых прошивках
        // пуст — тогда остаётся просто "cell".
        val operator = runCatching { tm?.networkOperatorName }.getOrNull()
            ?.lowercase()
            ?.replace(Regex("[^a-z0-9]+"), "")
            ?.take(16)
            .orEmpty()
        return if (operator.isEmpty()) "cell" else "cell-$operator"
    }
}
