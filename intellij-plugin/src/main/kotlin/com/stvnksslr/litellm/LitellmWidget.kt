package com.stvnksslr.litellm

import com.google.gson.Gson
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.util.SystemInfo
import com.intellij.openapi.wm.StatusBar
import com.intellij.openapi.wm.StatusBarWidget
import com.intellij.openapi.wm.StatusBarWidgetFactory
import com.intellij.util.concurrency.AppExecutorUtil
import java.io.File
import java.util.concurrent.TimeUnit

// ---- Binary resolution + env (mirrors the VS Code extension) ----------------

private fun resolveBinary(): String {
    val candidates = if (SystemInfo.isWindows) {
        val localAppData = System.getenv("LOCALAPPDATA")
        if (localAppData != null) {
            listOf(File(localAppData, "Programs/claude-code-litellm-plugin/claude-code-litellm-plugin.exe"))
        } else emptyList()
    } else {
        listOf(
            File(System.getProperty("user.home"), ".local/bin/claude-code-litellm-plugin"),
            File("/usr/local/bin/claude-code-litellm-plugin"),
        )
    }
    return candidates.firstOrNull { it.exists() }?.absolutePath
        ?: "claude-code-litellm-plugin" // let PATH resolve it
}

// Read the `env` block from ~/.claude/settings.json — GUI apps don't inherit the
// shell env, so the binary needs ANTHROPIC_BASE_URL / _AUTH_TOKEN passed in.
private fun claudeSettingsEnv(): Map<String, String> {
    val f = File(System.getProperty("user.home"), ".claude/settings.json")
    if (!f.canRead()) return emptyMap()
    return runCatching {
        @Suppress("UNCHECKED_CAST")
        val parsed = Gson().fromJson(f.readText(), Map::class.java) as Map<String, *>
        val env = parsed["env"]
        if (env is Map<*, *>) {
            env.entries.mapNotNull { (k, v) ->
                if (k is String && v is String) k to v else null
            }.toMap()
        } else emptyMap()
    }.getOrDefault(emptyMap())
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
        return if (json.startsWith("{")) {
            @Suppress("UNCHECKED_CAST")
            val parsed = Gson().fromJson(json, Map::class.java) as Map<String, *>
            parsed["text"] as? String ?: "LiteLLM: error"
        } else {
            "LiteLLM: error"
        }
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
