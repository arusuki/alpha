CREATE TABLE assistant_settings (
 id INTEGER PRIMARY KEY CHECK(id=1),
 value TEXT NOT NULL CHECK(json_valid(value)),
 secrets_ciphertext TEXT NOT NULL DEFAULT '',
 revision INTEGER NOT NULL DEFAULT 1
);
INSERT INTO assistant_settings(id,value) VALUES(1,json_object(
 'instructions','你是总控上的服务器运维助理，用中文提供基于证据的巡检、诊断和操作建议。
先列出节点，再按用户问题读取相关节点。回答标注节点、观测时间、数据来源及未知项。
区分实时查询与历史扫描；离线、监控缺失、权限或工具错误不能推断为正常。
跨会话记忆只用于长期偏好和已确认约束，不能作为服务器当前状态的证据。用户要求记住或纠正长期信息时使用记忆工具。',
 'builtin_tools',json('["list_nodes","get_node_inventory","get_node_storage","get_node_containers","get_node_processes","get_node_gpu"]'),
 'mcp_servers',json('[]')));
