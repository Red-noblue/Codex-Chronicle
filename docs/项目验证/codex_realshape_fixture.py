#!/usr/bin/env python3
# 本模块生成不含真实数据的 Codex 真实形状生命周期 fixture，并只在 fixture 内计算删除影响预览。

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import sqlite3
import tempfile
from typing import Any
from collections import OrderedDict


THREAD = "00000000-0000-4000-8000-000000000001"
CHILD = "00000000-0000-4000-8000-000000000002"
TURN = "00000000-0000-4000-8000-000000000011"
ITEM = "00000000-0000-4000-8000-000000000021"
ROLLOUT = f"sessions/2026/01/01/rollout-2026-01-01T00-00-00-{THREAD}.jsonl"
RETAINED_TRIGGER_STATE = {("queue_1.sqlite", "queued_thread_revisions")}


class UnsafeFixture(RuntimeError): pass


LIFECYCLE_SCHEMAS: dict[str, dict[str, list[tuple[str, str, bool, int]]]] = {
    "state_5.sqlite": {
        "threads": [("id","TEXT",True,1),("rollout_path","TEXT",True,0),("created_at","TEXT",True,0),("updated_at","TEXT",True,0),("source","TEXT",True,0),("model_provider","TEXT",True,0),("cwd","TEXT",True,0),("title","TEXT",True,0),("sandbox_policy","TEXT",True,0),("approval_mode","TEXT",True,0),("tokens_used","INTEGER",True,0),("has_user_event","INTEGER",True,0),("archived","INTEGER",True,0),("cli_version","TEXT",True,0),("first_user_message","TEXT",True,0),("memory_mode","TEXT",True,0),("preview","TEXT",True,0),("recency_at","TEXT",True,0),("recency_at_ms","INTEGER",True,0),("history_mode","TEXT",True,0),("is_pinned","INTEGER",True,0)],
        "thread_attachments": [("id","TEXT",False,1),("thread_id","TEXT",True,0),("attachment_type","TEXT",True,0),("identity_key","TEXT",True,0),("payload","TEXT",True,0),("created_at","INTEGER",True,0)],
        "thread_dynamic_tools": [("thread_id","TEXT",True,1),("position","INTEGER",True,2),("name","TEXT",True,0),("description","TEXT",True,0),("input_schema","TEXT",True,0),("defer_loading","INTEGER",True,0)],
        "thread_spawn_edges": [("parent_thread_id","TEXT",True,0),("child_thread_id","TEXT",True,1),("status","TEXT",True,0)],
        "thread_read_receipts": [("thread_id","TEXT",True,1),("revision","TEXT",True,0)],
    },
    "thread_history_1.sqlite": {
        "thread_history_projection_state": [("thread_id","TEXT",False,1),("next_rollout_byte_offset","INTEGER",True,0),("next_rollout_ordinal","INTEGER",True,0)],
        "thread_items": [("thread_id","TEXT",True,1),("turn_id","TEXT",True,2),("item_id","TEXT",True,3),("rollout_ordinal","INTEGER",True,0),("created_at_ms","INTEGER",True,0),("item_json","TEXT",True,0),("item_type","TEXT",True,0),("updated_at_ordinal","INTEGER",True,0)],
        "thread_realtime_items": [("thread_id","TEXT",True,1),("item_id","TEXT",True,2),("rollout_ordinal","INTEGER",True,0),("created_at_ms","INTEGER",True,0),("item_type","TEXT",True,0),("item_json","TEXT",True,0)],
        "thread_turns": [("thread_id","TEXT",True,1),("turn_id","TEXT",True,2),("rollout_ordinal","INTEGER",True,0),("status","TEXT",True,0)],
    },
}


def load_schema_snapshot() -> dict[str, dict[str, list[tuple[str, str, bool, int]]]]:
    """Load the checked-in local inventory projection: table, column, nullability and PK shape."""
    path=pathlib.Path(__file__).with_name("codex_schema_snapshot.json")
    data=json.loads(path.read_text(encoding="utf-8"))
    schemas: dict[str, dict[str, list[tuple[str,str,bool,int]]]]=OrderedDict()
    for database in data["databases"]:
        schemas[database["name"]]=OrderedDict()
        for table,details in database["tables"].items():
            schemas[database["name"]][table]=[(column["name"],column["type"],column["notnull"],column["primary_key_position"]) for column in details["columns"]]
    # Lifecycle-critical table names and reference columns must remain present.
    for database,tables in LIFECYCLE_SCHEMAS.items():
        for table,columns in tables.items():
            current=schemas.get(database,{}).get(table)
            expected_names={c[0] for c in columns}
            current_names={c[0] for c in current or []}
            if not current or not expected_names.issubset(current_names):
                raise UnsafeFixture(f"schema snapshot drift for lifecycle table {database}.{table}")
    return schemas


SCHEMAS=load_schema_snapshot()


def schema_fingerprint() -> str:
    canonical=json.dumps({db:{table:[list(c) for c in cols] for table,cols in tables.items()} for db,tables in SCHEMAS.items()},sort_keys=True,separators=(",",":"))
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def sqlite_semantics_fingerprint(root: pathlib.Path) -> str:
    semantics={}
    for database in SCHEMAS:
        con=sqlite3.connect(f"file:{root/database}?mode=ro",uri=True)
        try:
            semantics[database]=[list(row) for row in con.execute(
                "SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name"
            )]
        finally:
            con.close()
    canonical=json.dumps(semantics,sort_keys=True,separators=(",",":"))
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def assert_supported_fixture(root: pathlib.Path) -> None:
    marker=json.loads((root/"fixture-only.json").read_text(encoding="utf-8"))
    if marker.get("kind")!="codex-realshape-fixture-v1": raise UnsafeFixture("only generated real-shape fixtures are accepted")
    if marker.get("schema_sha256")!=schema_fingerprint(): raise UnsafeFixture("schema snapshot version drift; writes disabled")
    for database,tables in SCHEMAS.items():
        path=root/database
        if not path.is_file(): raise UnsafeFixture(f"required database missing: {database}")
        con=sqlite3.connect(f"file:{path}?mode=ro",uri=True)
        try:
            live={r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")}
            if live!=set(tables): raise UnsafeFixture(f"schema table drift in {database}")
            for table,columns in tables.items():
                actual=[(r[1],r[2] or "BLOB",bool(r[3]),r[5]) for r in con.execute(f"PRAGMA table_info({quote(table)})")]
                if actual!=columns: raise UnsafeFixture(f"schema column drift in {database}.{table}")
        finally: con.close()
    if marker.get("sqlite_semantics_sha256")!=sqlite_semantics_fingerprint(root):
        raise UnsafeFixture("schema semantic drift; writes disabled")


def quote(s: str) -> str:
    return '"' + s.replace('"', '""') + '"'


def create_db(path: pathlib.Path, tables: dict[str, list[tuple[str,str,bool,int]]]) -> None:
    con = sqlite3.connect(path)
    for table, cols in tables.items():
        definitions=[]
        pk=[c[0] for c in sorted(cols,key=lambda c:c[3]) if c[3]]
        for name, typ, notnull, _ in cols:
            definitions.append(f"{quote(name)} {typ or 'BLOB'}" + (" NOT NULL" if notnull else ""))
        if pk: definitions.append("PRIMARY KEY (" + ",".join(map(quote,pk)) + ")")
        con.execute(f"CREATE TABLE {quote(table)} ({','.join(definitions)})")
    con.commit(); con.close()


def install_lifecycle_triggers(root: pathlib.Path) -> None:
    """Install synthetic copies of the observed delete side effects used by the saga tests."""
    history = sqlite3.connect(root / "thread_history_1.sqlite")
    history.execute("""CREATE TRIGGER thread_realtime_items_projection_cleanup
        AFTER DELETE ON thread_history_projection_state
        BEGIN DELETE FROM thread_realtime_items WHERE thread_id = OLD.thread_id; END""")
    history.commit(); history.close()
    queue = sqlite3.connect(root / "queue_1.sqlite")
    queue.execute("CREATE UNIQUE INDEX queued_thread_revisions_thread_id ON queued_thread_revisions(thread_id)")
    for event, reference in (("INSERT", "NEW"), ("UPDATE", "NEW"), ("DELETE", "OLD")):
        queue.execute(f"""CREATE TRIGGER queued_items_revision_after_{event.lower()}
            AFTER {event} ON queued_items
            BEGIN
                INSERT INTO queued_thread_revisions (thread_id) VALUES ({reference}.thread_id)
                ON CONFLICT(thread_id) DO UPDATE
                SET revision = (SELECT COALESCE(MAX(revision), 0) + 1 FROM queued_thread_revisions);
            END""")
    queue.commit(); queue.close()


def insert(con: sqlite3.Connection, table: str, cols: list[tuple[str,str,bool,int]], values: dict[str,Any]) -> None:
    row=[]
    for name, typ, required, _ in cols:
        if name in values: row.append(values[name])
        elif required: row.append(0 if typ.upper() in {"INTEGER","INT","BIGINT"} else "fixture")
        else: row.append(None)
    con.execute(f"INSERT INTO {quote(table)} ({','.join(quote(c[0]) for c in cols)}) VALUES ({','.join('?' for _ in cols)})",row)


def create_fixture(root: pathlib.Path) -> pathlib.Path:
    root.mkdir(parents=True,exist_ok=False)
    for name,tables in SCHEMAS.items(): create_db(root/name,tables)
    install_lifecycle_triggers(root)
    (root/"fixture-only.json").write_text(json.dumps({
        "kind":"codex-realshape-fixture-v1",
        "schema_sha256":schema_fingerprint(),
        "sqlite_semantics_sha256":sqlite_semantics_fingerprint(root),
    }),encoding="utf-8")
    state=sqlite3.connect(root/"state_5.sqlite")
    base={"id":THREAD,"rollout_path":ROLLOUT,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:01:00Z","source":"cli","model_provider":"openai","cwd":"/fixture/project","title":"synthetic thread","sandbox_policy":"read-only","approval_mode":"never","tokens_used":42,"has_user_event":1,"archived":0,"cli_version":"fixture","first_user_message":"synthetic text","memory_mode":"enabled","preview":"synthetic preview","recency_at":"2026-01-01T00:01:00Z","recency_at_ms":1767225660000,"history_mode":"full","is_pinned":0}
    insert(state,"threads",SCHEMAS["state_5.sqlite"]["threads"],base)
    insert(state,"threads",SCHEMAS["state_5.sqlite"]["threads"],{**base,"id":CHILD,"rollout_path":ROLLOUT.replace(THREAD,CHILD),"title":"synthetic child"})
    insert(state,"thread_attachments",SCHEMAS["state_5.sqlite"]["thread_attachments"],{"id":"attachment-fixture","thread_id":THREAD,"attachment_type":"file","identity_key":"file:key","payload":"{}","created_at":1767225600})
    insert(state,"thread_dynamic_tools",SCHEMAS["state_5.sqlite"]["thread_dynamic_tools"],{"thread_id":THREAD,"position":0,"name":"fixture-tool","description":"synthetic","input_schema":"{}","defer_loading":0})
    insert(state,"thread_spawn_edges",SCHEMAS["state_5.sqlite"]["thread_spawn_edges"],{"parent_thread_id":THREAD,"child_thread_id":CHILD,"status":"completed"})
    insert(state,"thread_read_receipts",SCHEMAS["state_5.sqlite"]["thread_read_receipts"],{"thread_id":THREAD,"revision":"1"})
    # Synthetic records exercise less central thread references from the full local inventory.
    for table in ("guardian_review_feedback", "project_roots", "rollout_migration_skipped_rollouts"):
        schema=SCHEMAS["state_5.sqlite"].get(table,[])
        if any(c[0]=="thread_id" for c in schema):
            values={c[0]:THREAD for c in schema if c[0]=="thread_id"}
            insert(state,table,schema,values)
    state.commit(); state.close()
    history=sqlite3.connect(root/"thread_history_1.sqlite")
    for table,values in {
      "thread_history_projection_state":{"thread_id":THREAD,"next_rollout_byte_offset":128,"next_rollout_ordinal":2},
      "thread_items":{"thread_id":THREAD,"turn_id":TURN,"item_id":ITEM,"rollout_ordinal":1,"created_at_ms":1767225600000,"item_json":"{\"synthetic\":true}","item_type":"user_message","updated_at_ordinal":1},
      "thread_turns":{"thread_id":THREAD,"turn_id":TURN,"rollout_ordinal":1,"status":"completed"},
      "thread_realtime_items":{"thread_id":THREAD,"item_id":"realtime-fixture","rollout_ordinal":2,"created_at_ms":1767225660000,"item_type":"delta","item_json":"{}"},
    }.items(): insert(history,table,SCHEMAS["thread_history_1.sqlite"][table],values)
    history.commit(); history.close()
    # The remaining local Codex databases are represented with full table shapes and synthetic linked rows.
    for db_name in ("goals_1.sqlite","logs_2.sqlite","memories_1.sqlite","queue_1.sqlite"):
        con=sqlite3.connect(root/db_name)
        for table,schema in SCHEMAS[db_name].items():
            if db_name == "queue_1.sqlite" and table == "queued_thread_revisions":
                continue
            refs=[c[0] for c in schema if c[0] in {"thread_id","parent_thread_id","child_thread_id"}]
            if refs:
                insert(con,table,schema,{col:THREAD for col in refs})
        con.commit(); con.close()
    sessions=root/"sessions/2026/01/01"; sessions.mkdir(parents=True)
    rollout=root/ROLLOUT; rollout.write_text(json.dumps({"type":"session_meta","id":THREAD})+"\n"+json.dumps({"type":"event_msg","payload":{"thread_id":THREAD,"text":"synthetic"}})+"\n",encoding="utf-8")
    child_rollout=root/(ROLLOUT.replace(THREAD,CHILD)); child_rollout.write_text(json.dumps({"type":"session_meta","id":CHILD})+"\n",encoding="utf-8")
    (root/"session_index.jsonl").write_text(json.dumps({"id":THREAD,"thread_name":"synthetic"})+"\n"+json.dumps({"id":CHILD,"thread_name":"synthetic child"})+"\n",encoding="utf-8")
    return root


def preview(root: pathlib.Path, thread_id: str) -> dict[str,Any]:
    root=root.resolve()
    assert_supported_fixture(root)
    if (root/"active-thread.marker").exists(): raise UnsafeFixture("active thread: fail closed")
    state_path=root/"state_5.sqlite"; history_path=root/"thread_history_1.sqlite"
    files=[]; rows=[]; missing=[]
    con=sqlite3.connect(f"file:{state_path}?mode=ro",uri=True)
    tables={r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    if "threads" not in tables: raise UnsafeFixture("required state schema missing")
    row=con.execute("SELECT rollout_path FROM threads WHERE id=?",(thread_id,)).fetchone()
    if not row: raise UnsafeFixture("thread missing from state database")
    relpath=pathlib.PurePosixPath(row[0])
    if relpath.is_absolute() or ".." in relpath.parts: raise UnsafeFixture("unsafe rollout path")
    rollout=root.joinpath(*relpath.parts)
    if not rollout.is_file(): missing.append(str(relpath))
    else: files.append({"path":str(relpath),"sha256":hashlib.sha256(rollout.read_bytes()).hexdigest()})
    # Any current or future state table with a thread reference participates in the preview.
    for table in sorted(tables):
        cols=[r[1] for r in con.execute(f"PRAGMA table_info({quote(table)})")]
        if table == "threads":
            count=con.execute("SELECT COUNT(*) FROM threads WHERE id=?",(thread_id,)).fetchone()[0]
            if count: rows.append({"database":"state_5.sqlite","table":"threads","column":"id","count":count,"disposition":"delete"})
        refs=[c for c in cols if c in {"thread_id","parent_thread_id","child_thread_id"}]
        for col in refs:
            count=con.execute(f"SELECT COUNT(*) FROM {quote(table)} WHERE {quote(col)}=?",(thread_id,)).fetchone()[0]
            if count:
                disposition="retain_trigger_metadata" if ("state_5.sqlite",table) in RETAINED_TRIGGER_STATE else "delete"
                rows.append({"database":"state_5.sqlite","table":table,"column":col,"count":count,"disposition":disposition})
    con.close()
    con=sqlite3.connect(f"file:{history_path}?mode=ro",uri=True)
    tables={r[0] for r in con.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    for table in sorted(tables):
        cols=[r[1] for r in con.execute(f"PRAGMA table_info({quote(table)})")]
        if "thread_id" in cols:
            count=con.execute(f"SELECT COUNT(*) FROM {quote(table)} WHERE thread_id=?",(thread_id,)).fetchone()[0]
            if count:
                disposition="retain_trigger_metadata" if ("thread_history_1.sqlite",table) in RETAINED_TRIGGER_STATE else "delete"
                rows.append({"database":"thread_history_1.sqlite","table":table,"column":"thread_id","count":count,"disposition":disposition})
    con.close()
    for database in SCHEMAS:
        if database in {"state_5.sqlite", "thread_history_1.sqlite"}:
            continue
        con=sqlite3.connect(f"file:{root/database}?mode=ro",uri=True)
        for table, schema in SCHEMAS[database].items():
            for col in (c[0] for c in schema if c[0] in {"thread_id", "parent_thread_id", "child_thread_id"}):
                count=con.execute(f"SELECT COUNT(*) FROM {quote(table)} WHERE {quote(col)}=?",(thread_id,)).fetchone()[0]
                if count:
                    disposition="retain_trigger_metadata" if (database,table) in RETAINED_TRIGGER_STATE else "delete"
                    rows.append({"database":database,"table":table,"column":col,"count":count,"disposition":disposition})
        con.close()
    index=[]
    for n,line in enumerate((root/"session_index.jsonl").read_text(encoding="utf-8").splitlines(),1):
        try: item=json.loads(line)
        except json.JSONDecodeError: raise UnsafeFixture(f"invalid session index line {n}")
        if item.get("id")==thread_id: index.append(n)
    if missing: raise UnsafeFixture("state references missing rollout: "+", ".join(missing))
    if not files or not index: raise UnsafeFixture("incomplete rollout/index association")
    return {"thread_id":thread_id,"files":files,"database_rows":rows,"session_index_lines":index,"decision":"review_required","writes_performed":False}


def main() -> int:
    parser=argparse.ArgumentParser(description="Create synthetic real-shape Codex lifecycle fixture and preview impact")
    parser.add_argument("--root",type=pathlib.Path)
    args=parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="codex-realshape-") as td:
        root=create_fixture(args.root or pathlib.Path(td)/"fixture")
        print(json.dumps(preview(root,THREAD),ensure_ascii=False,indent=2))
    return 0

if __name__=="__main__": raise SystemExit(main())
