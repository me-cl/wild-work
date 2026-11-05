# 不混淆：手机壳只有两个类，而 gomobile 生成的 `Mobile` 类靠 JNI 的
# `Java_com_wildwork_mobile_Mobile_<method>` 命名约定绑定到 Go 符号，混淆会把
# 类名/方法名削掉，表现为运行时 UnsatisfiedLinkError。
-keep class com.wildwork.mobile.** { *; }
-keep class go.** { *; }

# WebView 的 JavascriptInterface 若将来引入，也同样不能被混淆（同上，靠名字反射）。
-keepclassmembers class * {
    @android.webkit.JavascriptInterface <methods>;
}
