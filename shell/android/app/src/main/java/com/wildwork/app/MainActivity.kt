package com.wildwork.app

import android.Manifest
import android.annotation.SuppressLint
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.util.Log
import android.view.View
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import com.wildwork.mobile.Mobile

/**
 * 面板外壳：一个全屏 WebView 指向 Go 侧起的 127.0.0.1 服务。
 *
 * 为什么不做原生 UI：面板是纯静态 embed 的 Web UI（R5），桌面端就是「系统浏览器 +
 * 托盘」，Android 端换成「Activity + WebView」，页面本身一行不改，也不存在两套实现。
 *
 * 启动顺序：Activity.onCreate → 启 DaemonService（幂等）→ 等 ACTION_READY 广播
 * （或直接读 Mobile.url()，服务若已在前台运行就不必等）→ WebView.loadUrl。
 */
class MainActivity : AppCompatActivity() {

    companion object {
        private const val TAG = "WildWork"
        private const val REQ_NOTIF = 100
    }

    private lateinit var web: WebView
    private lateinit var overlay: LinearLayout
    private lateinit var overlayText: TextView
    private var panelUrl: String = ""
    private var receiving = false

    private val receiver = object : BroadcastReceiver() {
        override fun onReceive(ctx: Context?, intent: Intent?) {
            when (intent?.action) {
                DaemonService.ACTION_READY -> {
                    val url = intent.getStringExtra(DaemonService.EXTRA_URL).orEmpty()
                    Log.i(TAG, "收到 READY: $url")
                    if (url.isNotEmpty()) loadPanel(url)
                }
                DaemonService.ACTION_FAILED -> {
                    val err = intent.getStringExtra(DaemonService.EXTRA_ERROR).orEmpty()
                    Log.e(TAG, "收到 FAILED: $err")
                    showError(err.ifEmpty { getString(R.string.start_failed) })
                }
            }
        }
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // 面板需要 JS 与 localStorage（R103 的「不适用账号默认收起」就存在 localStorage）。
        // 但 mix-content / 文件访问一概不开：它只跟本机环回说话。
        web = WebView(this).apply {
            settings.apply {
                javaScriptEnabled = true
                domStorageEnabled = true
                databaseEnabled = false
                allowFileAccess = false
                allowContentAccess = false
                mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
                setSupportZoom(false)
                builtInZoomControls = false
                loadWithOverviewMode = true
                useWideViewPort = true
                userAgentString = "$userAgentString WildWorkApp/Android"
            }
            // 登录流程需要新开页面（授权页 / 本机引导页）。WebView 默认把 target=_blank
            // 静默丢弃，所以：
            //   ① 面板里的「打开网页」经 JS 桥 WildWorkHost.openExternal 走系统浏览器；
            //   ② 页内 `<a target="_blank">`（如 glm/cline 的官网链接）也交给系统浏览器
            //      —— 在 App 内嵌页里打开厂商官网没有任何意义，还容易把面板顶掉。
            // 只有本机环回面板本身留在 WebView 里。
            addJavascriptInterface(WildWorkHost(this@MainActivity), "WildWorkHost")
            webViewClient = object : WebViewClient() {
                override fun onPageFinished(view: WebView?, url: String?) {
                    super.onPageFinished(view, url)
                    hideOverlay()
                }

                override fun shouldOverrideUrlLoading(
                    view: WebView?,
                    request: WebResourceRequest?
                ): Boolean {
                    val target = request?.url ?: return false
                    // 面板自身（本机服务）永远留在 WebView 里。
                    if (isPanelUrl(target)) return false
                    val scheme = target.scheme?.lowercase()
                    // 非 http(s)（tel:/mailto:/wxpayment: 等）一律交给系统处理，
                    // 在 WebView 里打开只会看到 ERR_UNKNOWN_URL_SCHEME。
                    return try {
                        startActivity(Intent(Intent.ACTION_VIEW, target))
                        true
                    } catch (t: Throwable) {
                        Log.w(TAG, "shouldOverrideUrlLoading: 无法处理 $scheme 链接", t)
                        false
                    }
                }

                override fun onReceivedError(
                    view: WebView?,
                    request: WebResourceRequest?,
                    error: WebResourceError?
                ) {
                    super.onReceivedError(view, request, error)
                    // 只关心主文档失败；子资源（favicon 等）失败不该盖掉整个面板。
                    if (request?.isForMainFrame == true) {
                        showError(
                            getString(R.string.load_failed, error?.description ?: "")
                        )
                    }
                }
            }
        }

        overlayText = TextView(this).apply {
            text = getString(R.string.loading_panel)
            setTextColor(0xFFDDDDDD.toInt())
            textSize = 14f
            setPadding(48, 48, 48, 48)
        }
        val bar = ProgressBar(this)
        overlay = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = android.view.Gravity.CENTER
            setBackgroundColor(0xFF000000.toInt())
            addView(bar)
            addView(overlayText)
        }

        setContentView(web)
        addContentView(
            overlay,
            LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT,
                LinearLayout.LayoutParams.MATCH_PARENT
            )
        )

        askNotificationPermission()

        // 服务可能已经在跑（Activity 被系统回收后重建、或从通知点进来）：
        // 先问一遍 Go 侧，省掉一次没必要的启动与等待。
        val running = try {
            Mobile.running()
        } catch (t: Throwable) {
            Log.w(TAG, "Mobile.running 失败", t)
            false
        }
        val url = if (running) {
            try { Mobile.url() } catch (t: Throwable) { "" }
        } else ""
        if (!url.isNullOrEmpty()) {
            Log.i(TAG, "服务已在运行: $url")
            loadPanel(url)
        } else {
            DaemonService.start(this)
            // 广播是在**服务**里发的：服务若早已 onCreate 过（进程还在、只是
            // Activity 被回收重建），这次 startService 不会再走 onCreate ⇒ 永远
            // 收不到 READY，页面就卡在「正在启动」。轮询兜底，不依赖广播时序。
            pollUntilReady()
        }
    }

    private fun pollUntilReady() {
        val handler = android.os.Handler(mainLooper)
        var attempts = 0
        val tick = object : Runnable {
            override fun run() {
                attempts++
                val u = try { Mobile.url() } catch (t: Throwable) { "" }
                if (!u.isNullOrEmpty()) {
                    loadPanel(u)
                    return
                }
                val err = try { Mobile.err() } catch (t: Throwable) { "" }
                if (!err.isNullOrEmpty()) {
                    showError(err)
                    return
                }
                if (attempts >= 120) { // 60 秒：冷启动装配通常 1~3 秒
                    showError(getString(R.string.load_failed, "启动超时"))
                    return
                }
                handler.postDelayed(this, 500)
            }
        }
        handler.postDelayed(tick, 300)
    }

    private fun loadPanel(url: String) {
        if (url == panelUrl && web.url == url && web.progress > 20) return
        panelUrl = url
        hideOverlay()
        web.loadUrl(url)
    }

    /**
     * 是否属于面板自身（本机 Go 服务）。
     *
     * 判据取 `panelUrl` 的 scheme+host:port，而不是写死 127.0.0.1 —— 端口由内核分配
     * （WILDWORK_LISTEN=127.0.0.1:0），写死端口会让整条「留在 WebView 里」的判定失效，
     * 面板里每一次跳转都会被丢去系统浏览器。
     */
    private fun isPanelUrl(uri: Uri): Boolean {
        if (panelUrl.isEmpty()) return false
        val panel = Uri.parse(panelUrl)
        return uri.scheme.equals(panel.scheme, ignoreCase = true) &&
            uri.host.equals(panel.host, ignoreCase = true) &&
            uri.port == panel.port
    }

    private fun showError(msg: String) {
        overlay.visibility = View.VISIBLE
        overlayText.text = msg
    }

    private fun hideOverlay() {
        overlay.visibility = View.GONE
    }

    /** Android 13+ 发前台服务通知要运行时授权；拒绝也不致命（服务照跑，只是没通知）。 */
    private fun askNotificationPermission() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
            == PackageManager.PERMISSION_GRANTED
        ) return
        ActivityCompat.requestPermissions(
            this,
            arrayOf(Manifest.permission.POST_NOTIFICATIONS),
            REQ_NOTIF
        )
    }

    override fun onStart() {
        super.onStart()
        if (!receiving) {
            val filter = IntentFilter().apply {
                addAction(DaemonService.ACTION_READY)
                addAction(DaemonService.ACTION_FAILED)
            }
            // Android 13+ 显式声明不对外导出；老版本没有 flag 参数的那重载。
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                registerReceiver(receiver, filter, Context.RECEIVER_NOT_EXPORTED)
            } else {
                @Suppress("UnspecifiedRegisterReceiverFlag")
                registerReceiver(receiver, filter)
            }
            receiving = true
        }
    }

    override fun onStop() {
        if (receiving) {
            try {
                unregisterReceiver(receiver)
            } catch (t: Throwable) {
                Log.w(TAG, "unregisterReceiver 失败", t)
            }
            receiving = false
        }
        super.onStop()
    }

    /** 返回键先退 WebView 历史（面板里点过弹层跳转时符合直觉），到根再退出。 */
    @Suppress("DEPRECATION")
    override fun onBackPressed() {
        if (web.canGoBack()) {
            web.goBack()
            return
        }
        super.onBackPressed()
    }

    override fun onDestroy() {
        try {
            web.destroy()
        } catch (t: Throwable) {
            Log.w(TAG, "web.destroy 失败", t)
        }
        super.onDestroy()
    }
}
