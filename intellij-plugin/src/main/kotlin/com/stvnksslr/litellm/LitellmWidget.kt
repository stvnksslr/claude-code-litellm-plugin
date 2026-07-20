package com.stvnksslr.litellm

import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.wm.StatusBar
import com.intellij.openapi.wm.StatusBarWidget
import com.intellij.openapi.wm.StatusBarWidgetFactory
import com.intellij.util.concurrency.AppExecutorUtil
import java.io.File
import java.util.concurrent.TimeUnit

// ---- Binary resolution + env (mirrors the VS Code extension) ----------------

private fun resolveBinary(): String {
    val candidates = listOf(
        File(System.getProperty("user.home"), ".local/bin/claude-code-litellm-plugin"),
        File("/usr/local/bin/claude-code-litellm-plugin"),
    )
    return candidates.firstOrNull { it.exists() }?.absolutePath
        ?: "claude-code-litellm-plugin" // let PATH resolve it
}

// Read the `env` block from ~/.claude/settings.json — GUI apps don't inherit the
// shell env, so the binary needs ANTHROPIC_BASE_URL / _AUTH_TOKEN passed in.
private fun claudeSettingsEnv(): Map<String, String> {
    val f = File(System.getProperty("user.home"), ".claude/settings.json")
    if (!f.canRead()) return emptyMap()
    return runCatching {
        val text = f.readText()
        // Grab the "env": { ... } object, then pull "k":"v" string pairs from it.
        val envBlock = Regex("\"env\"\\s*:\\s*\\{([^}]*)}").find(text)?.groupValues?.get(1)
            ?: return emptyMap()
        Regex("\"([^\"]+)\"\\s*:\\s*\"([^\"]*)\"").findAll(envBlock)
            .associate { it.groupValues[1] to it.groupValues[2] }
    }.getOrDefault(emptyMap())
}

// ---- Minimal flat-JSON scalar extraction ------------------------------------

private fun jsonString(json: String, key: String): String? =
    Regex("\"$key\"\\s*:\\s*\"([^\"]*)\"").find(json)?.groupValues?.get(1)

private fun jsonNumber(json: String, key: String): Double? =
    Regex("\"$key\"\\s*:\\s*(-?[0-9.]+)").find(json)?.groupValues?.get(1)?.toDoubleOrNull()

private fun jsonBool(json: String, key: String): Boolean =
    Regex("\"$key\"\\s*:\\s*(true|false)").find(json)?.groupValues?.get(1) == "true"

// Five-bucket circle gauge, matching the Go statusline glyphs.
private fun circleGlyph(percent: Double): String = when {
    percent >= 87.5 -> "●"
    percent >= 62.5 -> "◕"
    percent >= 37.5 -> "◑"
    percent >= 12.5 -> "◔"
    else -> "○"
}

// Build status bar text mirroring formatText() in the VS Code extension.
private fun formatText(json: String): String {
    val prefix = jsonString(json, "prefix") ?: "LiteLLM:"
    jsonString(json, "error")?.let { err ->
        val icon = if (err == "budget exceeded") "●" else "⚠"
        return "$prefix $icon $err"
    }

    val percent = jsonNumber(json, "percent") ?: 0.0
    val sb = StringBuilder("$prefix ${circleGlyph(percent)} ${Math.round(percent)}%")

    jsonString(json, "reset_time")?.let { reset ->
        val label = jsonString(json, "reset_label")?.let { "$it " } ?: ""
        sb.append(" ${label}reset: $reset")
    }
    if (jsonBool(json, "has_context")) {
        jsonNumber(json, "context_percent")?.let { sb.append(" | \uD83D\uDCD6 ${Math.round(it)}%") }
    }
    jsonString(json, "update_available")?.let { sb.append(" | update: $it") }
    return sb.toString()
}

// ---- Widget -----------------------------------------------------------------

private const val WIDGET_ID = "ClaudeCodeLitellm"
private const val REFRESH_SECONDS = 30L

class LitellmWidget : StatusBarWidget, StatusBarWidget.TextPresentation {

    @Volatile private var text: String = "LiteLLM: …"
    private var statusBar: StatusBar? = null
    private var future: java.util.concurrent.ScheduledFuture<*>? = null

    override fun ID(): String = WIDGET_ID
    override fun getPresentation(): StatusBarWidget.WidgetPresentation = this
    override fun getText(): String = text
    override fun getTooltipText(): String = "Claude Code LiteLLM budget"
    override fun getAlignment(): Float = 0.5f
    override fun getClickConsumer() = com.intellij.util.Consumer<java.awt.event.MouseEvent> { refresh() }

    override fun install(statusBar: StatusBar) {
        this.statusBar = statusBar
        future = AppExecutorUtil.getAppScheduledExecutorService()
            .scheduleWithFixedDelay({ refresh() }, 0, REFRESH_SECONDS, TimeUnit.SECONDS)
    }

    override fun dispose() {
        future?.cancel(true)
        future = null
    }

    private fun refresh() {
        val newText = runCatching { runBinary() }.getOrElse { "LiteLLM: binary not found" }
        text = newText
        ApplicationManager.getApplication().invokeLater { statusBar?.updateWidget(WIDGET_ID) }
    }

    private fun runBinary(): String {
        val pb = ProcessBuilder(resolveBinary(), "--json")
        pb.environment().putAll(claudeSettingsEnv())
        pb.redirectErrorStream(false)
        val proc = pb.start()
        proc.outputStream.use { it.write("{}".toByteArray()) } // binary reads stdin JSON
        val out = proc.inputStream.bufferedReader().readText()
        if (!proc.waitFor(5, TimeUnit.SECONDS)) {
            proc.destroyForcibly()
            return "LiteLLM: timeout"
        }
        val json = out.trim()
        return if (json.startsWith("{")) formatText(json) else "LiteLLM: error"
    }
}

class LitellmWidgetFactory : StatusBarWidgetFactory {
    override fun getId(): String = WIDGET_ID
    override fun getDisplayName(): String = "Claude Code LiteLLM"
    override fun isAvailable(project: Project): Boolean = true
    override fun createWidget(project: Project): StatusBarWidget = LitellmWidget()
    override fun disposeWidget(widget: StatusBarWidget) = widget.dispose()
    override fun canBeEnabledOn(statusBar: StatusBar): Boolean = true
}
