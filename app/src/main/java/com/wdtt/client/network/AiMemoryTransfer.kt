package com.wdtt.client

import android.content.Context
import android.content.Intent
import android.net.Uri
import androidx.core.content.FileProvider
import org.json.JSONObject
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * Перенос памяти адаптивной маскировки между устройствами.
 *
 * Зачем: то, что один телефон выучил в сети конкретного оператора, годится
 * другому телефону в той же сети. Обучение идёт по обратной связи от самого
 * канала, а канал у них общий — значит и вывод общий.
 *
 * Формат — один JSON со всеми сетями сразу:
 * ```
 * {"format":"qwdtt-aiobfs-memory","v":1,"nets":{"cell-beeline":{…},"wifi":{…}}}
 * ```
 * Внутри каждой сети лежит ровно то, что пишет ядро (go_client/aistate.go):
 * безразмерные веса бандита, сети и ручек. Ни ключей, ни адресов, ни объёмов
 * трафика — переносить такой файл безопасно.
 */
object AiMemoryTransfer {

    private const val FORMAT = "qwdtt-aiobfs-memory"
    private const val VERSION = 1
    private const val FILE_PREFIX = "aiobfs-"
    private const val FILE_SUFFIX = ".json"

    /** Каталог, куда ядро складывает память (совпадает с -ai-state). */
    fun memoryDir(context: Context): File = File(context.filesDir, "aiobfs")

    /** Сколько сетей уже выучено — для подписи к кнопке. */
    fun knownNetworks(context: Context): List<String> =
        memoryDir(context).listFiles()
            ?.mapNotNull { tagOf(it.name) }
            ?.sorted()
            .orEmpty()

    private fun tagOf(fileName: String): String? =
        if (fileName.startsWith(FILE_PREFIX) && fileName.endsWith(FILE_SUFFIX)) {
            fileName.removePrefix(FILE_PREFIX).removeSuffix(FILE_SUFFIX).ifEmpty { null }
        } else {
            null
        }

    /**
     * Собирает память всех сетей в один файл и отдаёт Intent для «Поделиться».
     * null — если памяти ещё нет.
     */
    fun buildShareIntent(context: Context): Intent? {
        val bundle = exportBundle(context) ?: return null

        val dir = File(context.cacheDir, "ai-memory").apply { mkdirs() }
        val stamp = SimpleDateFormat("yyyyMMdd_HHmmss", Locale.US).format(Date())
        val file = File(dir, "qwdtt-ai-memory_$stamp.json")
        file.writeText(bundle)

        val uri: Uri = FileProvider.getUriForFile(
            context, "${context.packageName}.fileprovider", file
        )
        return Intent(Intent.ACTION_SEND).apply {
            type = "application/json"
            putExtra(Intent.EXTRA_STREAM, uri)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
    }

    /** Собирает содержимое выгрузки. null — если памяти нет. */
    fun exportBundle(context: Context): String? {
        val files = memoryDir(context).listFiles()?.filter { tagOf(it.name) != null }.orEmpty()
        if (files.isEmpty()) return null

        val nets = JSONObject()
        for (file in files) {
            val tag = tagOf(file.name) ?: continue
            val text = runCatching { file.readText() }.getOrNull() ?: continue
            // Кладём разобранным объектом, а не строкой: так файл остаётся
            // читаемым и его нельзя случайно «завернуть» дважды.
            val parsed = runCatching { JSONObject(text) }.getOrNull() ?: continue
            nets.put(tag, parsed)
        }
        if (nets.length() == 0) return null

        return JSONObject().apply {
            put("format", FORMAT)
            put("v", VERSION)
            put("nets", nets)
        }.toString()
    }

    /** Результат импорта: сколько сетей принято и что пошло не так. */
    data class ImportResult(val imported: Int, val error: String? = null)

    /**
     * Принимает выгрузку. Файл приходит извне, поэтому проверяется всё:
     * формат, версия, имена сетей (они станут именами файлов) и то, что
     * внутри лежит объект, а не что попало.
     */
    fun importBundle(context: Context, text: String): ImportResult {
        val root = runCatching { JSONObject(text) }.getOrNull()
            ?: return ImportResult(0, "Это не файл памяти qWDTT")
        if (root.optString("format") != FORMAT) {
            return ImportResult(0, "Чужой формат файла")
        }
        if (root.optInt("v") != VERSION) {
            return ImportResult(0, "Версия файла не поддерживается")
        }
        val nets = root.optJSONObject("nets")
            ?: return ImportResult(0, "В файле нет ни одной сети")

        val dir = memoryDir(context).apply { mkdirs() }
        var imported = 0
        for (tag in nets.keys()) {
            val safeTag = sanitizeTag(tag) ?: continue
            val state = nets.optJSONObject(tag) ?: continue
            val target = File(dir, "$FILE_PREFIX$safeTag$FILE_SUFFIX")
            runCatching { target.writeText(state.toString()) }
                .onSuccess { imported++ }
        }
        return if (imported > 0) {
            ImportResult(imported)
        } else {
            ImportResult(0, "Не удалось прочитать ни одной сети")
        }
    }

    /**
     * Имя сети становится именем файла, а файл приходит снаружи — значит
     * «../» и прочее в имени недопустимы.
     */
    private fun sanitizeTag(tag: String): String? {
        val cleaned = tag.trim().replace(Regex("[^A-Za-z0-9_.-]+"), "-").trim('-', '.')
        return cleaned.ifEmpty { null }?.take(48)
    }
}
