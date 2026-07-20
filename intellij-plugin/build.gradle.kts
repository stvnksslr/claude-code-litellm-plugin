plugins {
    id("java")
    id("org.jetbrains.kotlin.jvm") version "1.9.25"
    id("org.jetbrains.intellij") version "1.17.4"
}

group = "com.stvnksslr"
version = "0.0.0-dev"

repositories { mavenCentral() }

intellij {
    version.set("2024.1")
    type.set("IC")
    plugins.set(emptyList())
}

tasks {
    patchPluginXml {
        sinceBuild.set("241")
        untilBuild.set("")
    }
    // No signing/publishing here — build the zip with `./gradlew buildPlugin`.
}

kotlin { jvmToolchain(17) }
