# 主机工作台

登录及恢复会话后首先进入总面板。总面板读取轻量扫描任务状态；进入存储的空间用量页时才加载完整快照。顶层模块为存储、进程管理、Agent 设置；账号管理属于平台。扫描记录与扫描配置位于存储的二级导航。Agent 设置沿用管理员权限；只读用户可以查看存储与进程。

扫描原始结果以 Blob 保存在浏览器 IndexedDB 中，按当前账号和结果地址隔离，不缓存解析后的索引。每次打开结果都发送带有 SHA-256 ETag 的条件请求；服务端按完整公开结果（含目录版本和容器归属）计算哈希，一致时返回无正文的 304，Worker 从本地文件重新解析。不一致时下载并替换缓存；认证或网络失败不会绕过校验展示缓存。缓存不可用或空间不足时仍可正常下载查看。存储模块的「本地结果缓存」展示各文件大小、总大小和删除操作；大小不含数据库开销。删除只影响浏览器缓存，并阻止删除前已开始的下载重新写回缓存。

进程页面按容器显示活动进程森林，可搜索命令、PID、路径和容器 ID，并可包含宿主机。页面可见时每三秒查询现有进程 API，离开模块或退出账号时停止轮询并取消请求。连接失败时明确标注错误；保留的数据标注为上次采集结果。

视觉采用白色表面、靛蓝主色 `#5b55db`、淡紫 `#efedfc`、薄荷绿 `#eaf6ef` 和浅杏色 `#fdf1e6`。统一细边框、圆角、线性图标和低幅度过渡。支持键盘焦点、移动布局及减少动画偏好。

## 欢迎插画

使用内置 image_gen 生成。最终资源：`dist/workspace-art.png`，用于总面板。图标仍以页面原生 SVG 实现。

最终提示词：

```text
Use case: stylized-concept
Asset type: decorative illustration in a host-management dashboard welcome panel; project-bound asset.
Primary request: a refined flat editorial illustration of a small organized computing workspace: stacked server storage blocks, a branching process flow, and one simple intelligent assistant spark, connected as a cohesive abstract system.
Scene/backdrop: solid very pale lavender #f0efff background, wide 3:2 composition, centered artwork with generous breathing room.
Style/medium: sophisticated geometric flat vector-like editorial art, crisp solid color blocks, playful but quiet, precise line accents, minimal detail.
Color palette: indigo #5b55db, pale lavender, mint #bce9d8, soft apricot #f5d4b1, small charcoal accents.
Constraints: no text, no letters, no logos, no watermark, no people, no photorealism, no 3D rendering, no gradients or heavy shadows. Whole composition visible and uncluttered, suitable at a small display size.
```

## 登录开屏

登录页使用独立的暖纸色 `#f6f5f1`、墨色 `#282a26` 和朱橙色 `#b94f2e`，搭配细网格、坐标注记和书法 α。布局、交互分别维护在 `dist/auth.css` 和 `dist/auth.js`，不依赖图片或外部字体。

α 使用一条连续 SVG 曲线，以固定角度的宽笔尖沿轨迹构造轮廓，每帧增加已书写部分。书写 2250ms，落笔前留白 220ms，完成后停留 420ms，再用 1100ms 移至左侧并显示右侧登录面板。小屏幕将符号收至表单上方。

每次页面加载仅首次需要登录时播放；已有会话直接进入工作台，退出或会话过期直接显示表单。可以跳过、按 Escape 或重播。符号绘制期间表单使用 inert 避免键盘进入不可见控件，面板开始出现即允许输入；减少动态效果时直接呈现完成布局，标签页隐藏时结束动画。初始化管理员、输入校验和后端错误沿用现有认证接口。
