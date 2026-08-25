#!/bin/bash

set -euo pipefail

: "${BASELINE_IMAGE_MAP:?BASELINE_IMAGE_MAP is required}"
PIN_RUNNER_REQUIRED="${PIN_RUNNER_REQUIRED:-true}"
if [ "$PIN_RUNNER_REQUIRED" = true ]; then
  : "${BASELINE_RUNNER_IMAGE:?BASELINE_RUNNER_IMAGE is required}"
fi
BASELINE_RUNNER_IMAGE="${BASELINE_RUNNER_IMAGE:-}"

awk \
  -v image_map="$BASELINE_IMAGE_MAP" \
  -v runner_image="$BASELINE_RUNNER_IMAGE" \
  -v runner_required="$PIN_RUNNER_REQUIRED" '
  function trim_yaml_value(value) {
    sub(/^[[:space:]]*/, "", value)
    sub(/[[:space:]]*$/, "", value)
    gsub(/^["'"'"']|["'"'"']$/, "", value)
    return value
  }

  function reset_document() {
    kind = ""
    name = ""
    top_metadata = 0
    container_type = ""
    section_indent = -1
    in_container = 0
    container_indent = -1
    container_name = ""
    container_image_line = 0
    runner_value_pending = 0
  }

  function is_workload(value) {
    return value == "Deployment" ||
      value == "DaemonSet" ||
      value == "StatefulSet" ||
      value == "CronJob" ||
      value == "Job" ||
      value == "Pod"
  }

  function buffer_line(value) {
    container_lines[++container_line_count] = value
  }

  function flush_container(    key, line_number, value) {
    if (!in_container) {
      return
    }
    key = kind SUBSEP name SUBSEP container_type SUBSEP container_name
    if (container_name == "" || container_image_line == 0 || !(key in images)) {
      print "ERROR: rendered workload image is missing from the baseline inventory: " kind "/" name " " container_type "/" container_name > "/dev/stderr"
      failed = 1
    } else {
      for (line_number = 1; line_number <= container_line_count; line_number++) {
        value = container_lines[line_number]
        if (line_number == container_image_line) {
          sub(/image:[[:space:]]*.*/, "image: \"" images[key] "\"", value)
        }
        print value
      }
      replaced[key]++
    }
    delete container_lines
    container_line_count = 0
    in_container = 0
    container_indent = -1
    container_name = ""
    container_image_line = 0
  }

  BEGIN {
    FS = "\t"
    while ((getline < image_map) > 0) {
      if (NF != 5 || $1 == "" || $2 == "" || $3 == "" || $4 == "" || $5 == "") {
        print "ERROR: invalid baseline image map row: " $0 > "/dev/stderr"
        exit 41
      }
      key = $1 SUBSEP $2 SUBSEP $3 SUBSEP $4
      if (key in images) {
        print "ERROR: duplicate baseline image map entry for " $1 "/" $2 " " $3 "/" $4 > "/dev/stderr"
        exit 41
      }
      images[key] = $5
      expected[key] = 1
    }
    close(image_map)
    reset_document()
  }

  /^---[[:space:]]*$/ {
    flush_container()
    reset_document()
    print
    next
  }

  /^kind:[[:space:]]*/ {
    kind = $0
    sub(/^kind:[[:space:]]*/, "", kind)
    kind = trim_yaml_value(kind)
  }

  /^metadata:[[:space:]]*$/ {
    top_metadata = 1
  }

  top_metadata && /^  name:[[:space:]]*/ {
    name = $0
    sub(/^  name:[[:space:]]*/, "", name)
    name = trim_yaml_value(name)
    top_metadata = 0
  }

  top_metadata && /^[^[:space:]]/ && !/^metadata:[[:space:]]*$/ {
    top_metadata = 0
  }

  {
    line = $0
    match(line, /^[[:space:]]*/)
    indent = RLENGTH
    stripped = substr(line, indent + 1)

    if (kind == "Deployment" && stripped ~ /^- name:[[:space:]]*RUNNER_IMAGE[[:space:]]*$/) {
      runner_value_pending = 1
    } else if (runner_value_pending && stripped ~ /^value:[[:space:]]*/) {
      sub(/value:[[:space:]]*.*/, "value: \"" runner_image "\"", line)
      stripped = substr(line, indent + 1)
      runner_replaced++
      runner_value_pending = 0
    } else if (runner_value_pending && stripped != "") {
      print "ERROR: RUNNER_IMAGE is not a scalar value in the rendered engine workload." > "/dev/stderr"
      exit 42
    }

    if (is_workload(kind) && stripped ~ /^(initContainers|containers):[[:space:]]*$/) {
      flush_container()
      container_type = stripped
      sub(/:.*/, "", container_type)
      section_indent = indent
      print line
      next
    }

    if (container_type != "" && ((!in_container && (indent == section_indent || indent == section_indent + 2)) || (in_container && indent == container_indent)) && stripped ~ /^- /) {
      flush_container()
      in_container = 1
      container_indent = indent
      field = stripped
      sub(/^- /, "", field)
      if (field ~ /^name:[[:space:]]*/) {
        sub(/^name:[[:space:]]*/, "", field)
        container_name = trim_yaml_value(field)
      } else if (field ~ /^image:[[:space:]]*/) {
        container_image_line = 1
      }
      buffer_line(line)
      next
    }

    if (container_type != "" && in_container && indent == container_indent + 2 && stripped ~ /^name:[[:space:]]*/) {
      field = stripped
      sub(/^name:[[:space:]]*/, "", field)
      container_name = trim_yaml_value(field)
    } else if (container_type != "" && in_container && indent == container_indent + 2 && stripped ~ /^image:[[:space:]]*/) {
      container_image_line = container_line_count + 1
    }

    if (container_type != "" && in_container && (stripped == "" || indent > container_indent)) {
      buffer_line(line)
      next
    }

    if (container_type != "" && stripped != "" && indent <= section_indent) {
      flush_container()
      container_type = ""
      section_indent = -1
    }

    print line
  }

  END {
    flush_container()
    if (runner_required == "true" && runner_replaced != 1) {
      print "ERROR: expected to pin exactly one RUNNER_IMAGE value, replaced " runner_replaced "." > "/dev/stderr"
      failed = 1
    }
    for (key in expected) {
      if (replaced[key] != 1) {
        split(key, fields, SUBSEP)
        print "ERROR: expected to pin exactly one image for " fields[1] "/" fields[2] " " fields[3] "/" fields[4] ", replaced " replaced[key] "." > "/dev/stderr"
        failed = 1
      }
    }
    if (failed) {
      exit 42
    }
  }
'
