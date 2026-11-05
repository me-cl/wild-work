package com.wildwork.app

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.util.Log
import java.io.File
import com.wildwork.mobile.Mobile

/**
 * 前台服务：承载 Go 侧常驻服务（daemon + 调度器）。
 *
 * 为什么必须挂前台服务：Android 会把裸后台进程在几分钟内回收，Doze 还会把定时器
 * 批处理掉。wild-work 是「常驻代理 + 定时签到」，被回收等于整条链路停摆。挂上前台
 * 服务（带常驻通知）才能合法地长期存活。
 *
 * 生命周期约定：
 *  - onCreate → Mobile.start（幂等），成功后广播 [ACTION_READY] 带 URL，失败广播 [ACTION_FAILED] 带原因
 *  - onStartCommand 返回 START_STICKY：被系统回收后重建，onCreate 里会重新 Start
 *  - onDestroy → Mobile.stop（幂等）
 */
class DaemonService : Service() {

    companion object {
        private const val TAG = "WildWork"
        private const val CHANNEL_ID = "wildwork_daemon"
        private const val NOTIF_ID = 1001
        private const val ACTION_STOP_SELF = "com.wildwork.app.STOP_SELF"

        const val ACTION_READY = "com.wildwork.app.READY"
        const val ACTION_FAILED = "com.wildwork.app.FAILED"
        const val EXTRA_URL = "url"
        const val EXTRA_ERROR = "error"

        /** 启动服务（Android 8+ 必须走 startForegroundService）。 */
        fun start(ctx: Context) {
            val i = Intent(ctx, DaemonService::class.java)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                ctx.startForegroundService(i)
            } else {
                ctx.startService(i)
            }
        }

        fun stop(ctx: Context) {
            ctx.stopService(Intent(ctx, DaemonService::class.java))
        }
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        createChannel()
        // 必须在 5 秒内 startForeground，否则系统抛 ForegroundServiceDidNotStartInTime。
        // 故先挂通知（标题还是「正在启动」），再去跑 Go 侧装配。
        startForeground(NOTIF_ID, buildNotification(getString(R.string.loading_panel)))
        // 装配要读 auths/、扫 data/、起调度器，冷启动实测 1~3 秒（账号多时更久）。
        // 直接跑在 onCreate 里就是主线程阻塞：ANR 阈值是 5 秒，账号池大了必踩。
        //
        // 注意：watchPanelQuit() 必须等**装配成功之后**再挂（见 startDaemon 末尾）。
        // 放在这里会有一个启动竞态：装配期间 Mobile.running() 还是 false，
        // 2 秒后轮询就把服务停掉，而装配线程随后成功、广播没人收，留下一个
        // 没有前台服务兜底的孤儿 daemon（随时被系统回收）。
        Thread({ startDaemon() }, "wildwork-boot").start()
    }

    /**
     * 面板上的「退出」按钮走 `POST /api/quit`。在嵌入式宿主里它只停 Go 侧服务
     * （见 app.Options.Embedded）：**不会**杀掉进程，也**不会**停这个前台服务——
     * 不停的话通知栏会一直挂着「运行中」，用户以为没退出。
     *
     * 这里每 2 秒问一次 Go 侧；服务已停且不是我们自己发起的 stop，就收摊。
     * 不用反向绑定（gomobile 不支持 Kotlin→Go 回调），也不用轮询 HTTP：
     * `Mobile.running()` 是进程内一个布尔值，开销可忽略。
     */
    private fun watchPanelQuit() {
        val handler = android.os.Handler(mainLooper)
        val tick = object : Runnable {
            override fun run() {
                val running = try { Mobile.running() } catch (t: Throwable) { true }
                if (!running) {
                    Log.i(TAG, "面板已请求退出，停止前台服务")
                    stopSelf()
                    return
                }
                handler.postDelayed(this, 2000)
            }
        }
        handler.postDelayed(tick, 2000)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP_SELF) {
            // 通知上的「停止」：先停 Go 侧再停服务本身（onDestroy 会再兜一次，幂等）。
            stopSelf()
            return START_NOT_STICKY
        }
        // 系统回收后重建：onCreate 已经重新 Start 了，这里无需重复。
        return START_STICKY
    }

    override fun onDestroy() {
        Log.i(TAG, "DaemonService.onDestroy → Mobile.stop()")
        try {
            Mobile.stop()
        } catch (t: Throwable) {
            Log.w(TAG, "Mobile.stop 失败", t)
        }
        super.onDestroy()
    }

    private fun startDaemon() {
        // Go 侧所有可写目录都在 filesDir 下：APK 的 assets 只读，而 config.json /
        // auths/ / data/ 都要可写。Context.filesDir 随 APK 卸载一起清理，
        // 符合「账号凭据属于这个 app」的直觉。
        val home = File(filesDir.absolutePath).apply { mkdirs() }.absolutePath
        Log.i(TAG, "启动 daemon，home=$home")

        val url: String = try {
            Mobile.start(home) ?: ""
        } catch (t: Throwable) {
            Log.e(TAG, "Mobile.start 抛异常", t)
            ""
        }

        if (url.isEmpty()) {
            val err = try { Mobile.err() } catch (t: Throwable) { t.message ?: "unknown" }
            Log.e(TAG, "启动失败: $err")
            notify(ACTION_FAILED, EXTRA_ERROR, if (err.isNullOrEmpty()) getString(R.string.start_failed) else err)
            // 失败就别赖在原地占着通知：停掉前台状态，让用户看到失败并重试。
            stopSelf()
            return
        }

        Log.i(TAG, "daemon 已启动: $url")
        // 更新通知（附带「打开面板」的跳转），再广播给 Activity。
        updateNotification(url)
        notify(ACTION_READY, EXTRA_URL, url)
        // 装配成功之后才挂「面板退出」轮询：此前 running() 恒 false，
        // 挂早了会把正在装配的服务误判成「用户已退出」。
        runOnMain { watchPanelQuit() }
    }

    /** 把 UI 操作（Handler、通知更新）挪回主线程——startDaemon 跑在后台线程。 */
    private fun runOnMain(block: () -> Unit) {
        android.os.Handler(mainLooper).post(block)
    }

    private fun notify(action: String, key: String, value: String) {
        val i = Intent(action).setPackage(packageName).putExtra(key, value)
        sendBroadcast(i)
    }

    private fun createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val nm = getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL_ID) != null) return
        val ch = NotificationChannel(
            CHANNEL_ID,
            getString(R.string.notif_channel_name),
            // LOW：常驻状态通知，不需要响声与横幅——它每时每刻都在。
            NotificationManager.IMPORTANCE_LOW
        )
        ch.setShowBadge(false)
        nm.createNotificationChannel(ch)
    }

    private fun buildNotification(text: String): Notification {
        val open = PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val stop = PendingIntent.getService(
            this, 1,
            Intent(this, DaemonService::class.java).setAction(ACTION_STOP_SELF),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.notif_title))
            .setContentText(text)
            .setSmallIcon(android.R.drawable.stat_sys_download_done)
            .setContentIntent(open)
            .addAction(
                Notification.Action.Builder(null, getString(R.string.action_stop), stop).build()
            )
            .setOngoing(true)
            .build()
    }

    private fun updateNotification(url: String) {
        val nm = getSystemService(NotificationManager::class.java)
        nm.notify(NOTIF_ID, buildNotification(url))
    }
}
