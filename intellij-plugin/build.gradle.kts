import org.jetbrains.kotlin.gradle.dsl.JvmTarget
import org.jetbrains.kotlin.gradle.tasks.KotlinCompile

plugins {
    id("java")
    id("org.jetbrains.kotlin.jvm") version "1.9.25"
    id("org.jetbrains.intellij") version "1.17.4"
}

group = "com.stvnksslr"
// Stamped by the release workflow: ./gradlew buildPlugin -PpluginVersion=X.Y.Z
version = providers.gradleProperty("pluginVersion").getOrElse("0.0.0-dev")

repositories { mavenCentral() }

intellij {
    version.set("2024.1")
    type.set("IC")
    plugins.set(emptyList())
}

// since-build 241 runs on JBR 17, so emit Java 17 bytecode. Compiling with
// whatever JDK runs Gradle (mise pins 21) avoids needing a separate JDK 17.
java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
}
tasks.withType<KotlinCompile>().configureEach {
    compilerOptions { jvmTarget.set(JvmTarget.JVM_17) }
}

tasks {
    patchPluginXml {
        sinceBuild.set("241")
        untilBuild.set("")
    }
    // No settings UI, so there's nothing to index (it also launches a headless IDE).
    buildSearchableOptions { enabled = false }
    // No signing/publishing here — build the zip with `./gradlew buildPlugin`.
}
