rootProject.extra.apply {
    set("androidPluginVersion", "7.1.0")
    set("kotlinVersion", "1.6.10")
}

repositories {
    google()
    mavenCentral()
    gradlePluginPortal()
    maven(url = "https://jitpack.io")
}