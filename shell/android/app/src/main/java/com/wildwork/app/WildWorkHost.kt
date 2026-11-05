package com.wildwork.app

import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.util.Log
import android.webkit.JavascriptInterface

/**
 * 面板页面 ↔ 外壳的 JS 桥（挂在 `window.WildWorkHost`）。
 *
 * 为什么需要它：面板是桌面端同一份 Web UI，登录流程里有 `window.open(url, "_blank")`。
 * Android WebView 默认不处理 `target=_blank`（没开 setSupportMultipleWindows 时它
 * **静默丢弃**），即便打开也只是在 App 内再开一个 WebView —— 而用户要的是「在系统浏览器里
 * 打开订单/授权页」（手机上有登录态、有短信、有扫码，App 内嵌页做不到）。
 *
 * 所以外壳往页面注入这个对象，前端 openExternal() 优先调它；拿不到桥（桌面浏览器里
 * 当然没有）就退回 window.open，两边行为一致。
 *
 * ⚠️ 安全边界：只接受 http/https，绝不 `file://` / `content://` / `intent://`。
 * 页面内容虽然是我们自己的静态资源，但登录 URL 由后端决定、也可能来自用户粘贴，
 * 不做协议白名单等于把任意 Intent 的发起权交给了页面。
 */
class WildWorkHost(private val ctx: Context) {

    companion object {
        private const val TAG = "WildWork"
    }

    /** 面板探测本桥是否可用（前端也可以直接判 typeof）。 */
    @JavascriptInterface
    fun version(): Int = 1

    /**
     * 在系统浏览器打开 url。返回 true 表示已转交；false 表示被拒（非法协议/无浏览器）。
     *
     * 注意这是 JS 桥方法，**没有** UI 线程之外的限制问题：startActivity 本身线程安全，
     * 但为了与其它桥实现的习惯一致仍走主线程。
     */
    @JavascriptInterface
    fun openExternal(url: String?): Boolean {
        val raw = url?.trim().orEmpty()
        if (raw.isEmpty()) return false
        val uri = try {
            Uri.parse(raw)
        } catch (t: Throwable) {
            Log.w(TAG, "openExternal: 非法 URL", t)
            return false
        }
        val scheme = uri.scheme?.lowercase()
        if (scheme != "http" && scheme != "https") {
            // 不把任意 scheme 交给系统：file:// 会暴露本地文件，intent:// 可直接调起
            // 任意组件（含系统设置页）。面板只需要 https 授权页。
            Log.w(TAG, "openExternal: 拒绝非 http(s) 协议 scheme=$scheme")
            return false
        }
        return try {
            val intent = Intent(Intent.ACTION_VIEW, uri).apply {
                addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            }
            ctx.startActivity(intent)
            true
        } catch (e: ActivityNotFoundException) {
            Log.w(TAG, "openExternal: 没有可处理该链接的应用", e)
            false
        } catch (t: Throwable) {
            Log.w(TAG, "openExternal: 启动外部浏览器失败", t)
            false
        }
    }
}
