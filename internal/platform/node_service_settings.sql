CREATE TABLE node_service_settings (
 name TEXT PRIMARY KEY CHECK(name IN ('tetragon','dram-bw','rootless-docker')),
 config TEXT NOT NULL CHECK(json_valid(config)),
 CHECK(name <> 'dram-bw' OR (
  json_type(config,'$.dram') IS 'object'
  AND json_extract(config,'$.dram.backend') IS NOT NULL
  AND json_extract(config,'$.dram.backend') IN ('amd-rome','auto','mock')
  AND json_type(config,'$.dram.interval_us') IS 'integer'
  AND json_extract(config,'$.dram.interval_us') BETWEEN 100 AND 10000000
  AND json_type(config,'$.dram.peak_gbps') IN ('integer','real')
  AND json_extract(config,'$.dram.peak_gbps') IS NOT NULL
  AND json_extract(config,'$.dram.peak_gbps') BETWEEN 0 AND 1000000
 ))
);
