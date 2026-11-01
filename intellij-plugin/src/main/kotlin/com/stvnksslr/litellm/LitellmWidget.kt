package com.stvnksslr.litellm

import com.google.gson.Gson
import com.google.gson.JsonSyntaxException
import com.intellij.execution.ExecutionException
import com.intellij.execution.configurations.GeneralCommandLine
import com.intellij.execution.process.CapturingProcessHandler
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.util.SystemInfo
import com.intellij.openapi.wm.StatusBar
import com.intellij.openapi.wm.StatusBarWidget
import com.intellij.openapi.wm.StatusBarWidgetFactory
import com.intellij.util.concurrency.AppExecutorUtil
import java.io.File
import java.util.Locale
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

// Kept in sync with the VS Code extension (vscode-extension/src/extension.ts):
// same binary candidates, timeout, states and tooltip content. Loading the `env`
// block of Claude Code's settings.json is done by the binary itself (--json).

private const val BINARY = "claude-code-litellm-plugin"
private const val WIDGET_ID = "ClaudeCodeLitellm"
private const val REFRESH_SECONDS = 30L
private const val TIMEOUT_MS = 10_000

/** Fields emitted by `claude-code-litellm-plugin --json` (all optional). */
private data class StatusJson(
    val text: String? = null,
    val percent: Double? = null,
    val spend: Double? = null,
    val max_budget: Double? = null,
    val has_budget: Boolean? = null,
    val reset_label: String? = null,
    val reset_time: String? = null,
    val update_available: String? = null,
    val context_percent: Double? = null,
    val has_context: Boolean? = null,
    val error: String? = null,
    val version: String? = null,
)

private data class WidgetState(val text: String, val tooltip: String)

/** Known install locations (installer, Homebrew, Scoop), then PATH via the bare name. */
private fun resolveBinary(): String {
    val home = System.getProperty("user.home")
    val candidates = if (SystemInfo.isWindows) {
        val localAppData = System.getenv("LOCALAPPDATA") ?: File(home, "AppData/Local").path
        listOf(
            File(localAppData, "Programs/$BINARY/$BINARY.exe"),
            File(home, "scoop/shims/$BINARY.exe"),
        )
    } else {
        listOf(
            File(home, ".local/bin/$BINARY"),
            File("/opt/homebrew/bin/$BINARY"),
            File("/usr/local/bin/$BINARY"),
            File("/home/linuxbrew/.linuxbrew/bin/$BINARY"),
        )
    }
    return candidates.firstOrNull { it.canExecute() }?.absolutePath ?: BINARY
}

private fun money(v: Double) = String.format(Locale.ROOT, "$%.2f", v)

private fun tooltipFor(s: StatusJson, binary: String): String {
    val lines = mutableListOf<String>()
    s.error?.let { lines += "Status: $it" }
    if (s.has_budget == true && s.spend != null && s.max_budget != null) {
        lines += "Spend: ${money(s.spend)} / ${money(s.max_budget)}"
        if (s.error == null) lines += String.format(Locale.ROOT, "Usage: %.1f%%", s.percent ?: 0.0)
    }
    if (s.error == null && !s.reset_time.isNullOrEmpty()) {
        val label = if (s.reset_label.isNullOrEmpty()) "" else " (${s.reset_label})"
        lines += "Reset$label: ${s.reset_time}"
    }
    if (s.has_context == true && s.context_percent != null) {
        lines += String.format(Locale.ROOT, "Context window: %.0f%%", s.context_percent)
    }
    s.update_available?.let { lines += "Update available: $it (re-run the installer, or brew/scoop upgrade)" }
    if (s.error != null) lines += "Diagnose: run `$BINARY doctor` in a terminal"
    lines += "Binary: $binary" + (s.version?.let { " ($it)" } ?: "")
    // Swing tooltips render HTML; escape and keep line breaks.
    return "<html>" + lines.joinToString("<br>") { it.replace("&", "&amp;").replace("<", "&lt;") } + "</html>"
}

/** Runs the binary once. Never call on the EDT. */
private fun fetchState(): WidgetState {
    val binary = resolveBinary()
    val cmd = GeneralCommandLine(binary, "--json")
        // CONSOLE adds the login-shell environment IntelliJ loads on macOS, so a
        // Dock-launched IDE sees the same PATH/env as a terminal.
        .withParentEnvironmentType(GeneralCommandLine.ParentEnvironmentType.CONSOLE)
        .withCharset(Charsets.UTF_8)
    val output = try {
        val handler = CapturingProcessHandler(cmd)
        handler.processInput.use { it.write("{}".toByteArray()) } // binary reads Claude Code's stdin JSON
        handler.runProcess(TIMEOUT_MS, true)
    } catch (e: ExecutionException) {
        return WidgetState(
            "LiteLLM: binary not found",
            "$BINARY was not found ($binary). Install it with the one-line installer (see the README).",
        )
    }
    if (output.isTimeout) {
        return WidgetState("LiteLLM: timeout", "$binary did not answer within ${TIMEOUT_MS / 1000}s. Run `$BINARY doctor` in a terminal.")
    }
    val parsed = try {
        Gson().fromJson(output.stdout.trim(), StatusJson::class.java)
    } catch (e: JsonSyntaxException) {
        null
    } ?: return WidgetState("LiteLLM: error", "$binary returned unexpected output. Run `$BINARY doctor` in a terminal.")
    if (parsed.version.isNullOrEmpty()) {
        return WidgetState("LiteLLM: update binary", "$binary is older than this plugin. Re-run the installer (or brew/scoop upgrade).")
    }
    return WidgetState(parsed.text ?: "LiteLLM: error", tooltipFor(parsed, binary))
}

class LitellmWidget : StatusBarWidget, StatusBarWidget.TextPresentation {

    @Volatile private var state = WidgetState("LiteLLM: …", "Claude Code LiteLLM budget")
    private var statusBar: StatusBar? = null
    private var future: ScheduledFuture<*>? = null
    private val inFlight = AtomicBoolean(false)

    override fun ID(): String = WIDGET_ID
    override fun getPresentation(): StatusBarWidget.WidgetPresentation = this
    override fun getText(): String = state.text
    override fun getTooltipText(): String = state.tooltip
    override fun getAlignment(): Float = 0.5f

    // Click handlers run on the EDT: hand the (blocking) refresh to a pooled thread.
    override fun getClickConsumer() = com.intellij.util.Consumer<java.awt.event.MouseEvent> {
        ApplicationManager.getApplication().executeOnPooledThread { refresh() }
    }

    override fun install(statusBar: StatusBar) {
        this.statusBar = statusBar
        future = AppExecutorUtil.getAppScheduledExecutorService()
            .scheduleWithFixedDelay({ refresh() }, 0, REFRESH_SECONDS, TimeUnit.SECONDS)
    }

    override fun dispose() {
        future?.cancel(true)
        future = null
        statusBar = null
    }

    private fun refresh() {
        if (!inFlight.compareAndSet(false, true)) return
        try {
            state = fetchState()
        } finally {
            inFlight.set(false)
        }
        ApplicationManager.getApplication().invokeLater { statusBar?.updateWidget(WIDGET_ID) }
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
