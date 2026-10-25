import datanode_pb2 as _datanode_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Iterable as _Iterable, Mapping as _Mapping, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RegisterRequest(_message.Message):
    __slots__ = ("endpoint", "capacity_bytes")
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    CAPACITY_BYTES_FIELD_NUMBER: _ClassVar[int]
    endpoint: _datanode_pb2.DataNodeEndpoint
    capacity_bytes: int
    def __init__(self, endpoint: _Optional[_Union[_datanode_pb2.DataNodeEndpoint, _Mapping]] = ..., capacity_bytes: _Optional[int] = ...) -> None: ...

class RegisterResponse(_message.Message):
    __slots__ = ("datanode_id", "heartbeat_interval_seconds")
    DATANODE_ID_FIELD_NUMBER: _ClassVar[int]
    HEARTBEAT_INTERVAL_SECONDS_FIELD_NUMBER: _ClassVar[int]
    datanode_id: str
    heartbeat_interval_seconds: int
    def __init__(self, datanode_id: _Optional[str] = ..., heartbeat_interval_seconds: _Optional[int] = ...) -> None: ...

class HeartbeatRequest(_message.Message):
    __slots__ = ("datanode_id", "capacity_bytes", "used_bytes", "active_transfers")
    DATANODE_ID_FIELD_NUMBER: _ClassVar[int]
    CAPACITY_BYTES_FIELD_NUMBER: _ClassVar[int]
    USED_BYTES_FIELD_NUMBER: _ClassVar[int]
    ACTIVE_TRANSFERS_FIELD_NUMBER: _ClassVar[int]
    datanode_id: str
    capacity_bytes: int
    used_bytes: int
    active_transfers: int
    def __init__(self, datanode_id: _Optional[str] = ..., capacity_bytes: _Optional[int] = ..., used_bytes: _Optional[int] = ..., active_transfers: _Optional[int] = ...) -> None: ...

class HeartbeatResponse(_message.Message):
    __slots__ = ("commands", "registered")
    COMMANDS_FIELD_NUMBER: _ClassVar[int]
    REGISTERED_FIELD_NUMBER: _ClassVar[int]
    commands: _containers.RepeatedCompositeFieldContainer[Command]
    registered: bool
    def __init__(self, commands: _Optional[_Iterable[_Union[Command, _Mapping]]] = ..., registered: bool = ...) -> None: ...

class Command(_message.Message):
    __slots__ = ("replicate", "delete")
    REPLICATE_FIELD_NUMBER: _ClassVar[int]
    DELETE_FIELD_NUMBER: _ClassVar[int]
    replicate: ReplicateCommand
    delete: DeleteCommand
    def __init__(self, replicate: _Optional[_Union[ReplicateCommand, _Mapping]] = ..., delete: _Optional[_Union[DeleteCommand, _Mapping]] = ...) -> None: ...

class ReplicateCommand(_message.Message):
    __slots__ = ("block_id", "targets")
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    TARGETS_FIELD_NUMBER: _ClassVar[int]
    block_id: str
    targets: _containers.RepeatedCompositeFieldContainer[_datanode_pb2.DataNodeEndpoint]
    def __init__(self, block_id: _Optional[str] = ..., targets: _Optional[_Iterable[_Union[_datanode_pb2.DataNodeEndpoint, _Mapping]]] = ...) -> None: ...

class DeleteCommand(_message.Message):
    __slots__ = ("block_ids",)
    BLOCK_IDS_FIELD_NUMBER: _ClassVar[int]
    block_ids: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, block_ids: _Optional[_Iterable[str]] = ...) -> None: ...

class BlockInfo(_message.Message):
    __slots__ = ("block_id", "size", "checksum_sha256")
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    SIZE_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_SHA256_FIELD_NUMBER: _ClassVar[int]
    block_id: str
    size: int
    checksum_sha256: str
    def __init__(self, block_id: _Optional[str] = ..., size: _Optional[int] = ..., checksum_sha256: _Optional[str] = ...) -> None: ...

class BlockReportRequest(_message.Message):
    __slots__ = ("datanode_id", "blocks")
    DATANODE_ID_FIELD_NUMBER: _ClassVar[int]
    BLOCKS_FIELD_NUMBER: _ClassVar[int]
    datanode_id: str
    blocks: _containers.RepeatedCompositeFieldContainer[BlockInfo]
    def __init__(self, datanode_id: _Optional[str] = ..., blocks: _Optional[_Iterable[_Union[BlockInfo, _Mapping]]] = ...) -> None: ...

class BlockReportResponse(_message.Message):
    __slots__ = ("orphan_block_ids",)
    ORPHAN_BLOCK_IDS_FIELD_NUMBER: _ClassVar[int]
    orphan_block_ids: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, orphan_block_ids: _Optional[_Iterable[str]] = ...) -> None: ...

class BlockReceivedRequest(_message.Message):
    __slots__ = ("datanode_id", "block")
    DATANODE_ID_FIELD_NUMBER: _ClassVar[int]
    BLOCK_FIELD_NUMBER: _ClassVar[int]
    datanode_id: str
    block: BlockInfo
    def __init__(self, datanode_id: _Optional[str] = ..., block: _Optional[_Union[BlockInfo, _Mapping]] = ...) -> None: ...

class BlockReceivedResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class EditLogEntry(_message.Message):
    __slots__ = ("txid", "timestamp_unix_ms", "mkdir", "delete", "allocate_block", "commit_block", "finalize_file")
    TXID_FIELD_NUMBER: _ClassVar[int]
    TIMESTAMP_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    MKDIR_FIELD_NUMBER: _ClassVar[int]
    DELETE_FIELD_NUMBER: _ClassVar[int]
    ALLOCATE_BLOCK_FIELD_NUMBER: _ClassVar[int]
    COMMIT_BLOCK_FIELD_NUMBER: _ClassVar[int]
    FINALIZE_FILE_FIELD_NUMBER: _ClassVar[int]
    txid: int
    timestamp_unix_ms: int
    mkdir: MkdirOp
    delete: DeleteOp
    allocate_block: AllocateBlockOp
    commit_block: CommitBlockOp
    finalize_file: FinalizeFileOp
    def __init__(self, txid: _Optional[int] = ..., timestamp_unix_ms: _Optional[int] = ..., mkdir: _Optional[_Union[MkdirOp, _Mapping]] = ..., delete: _Optional[_Union[DeleteOp, _Mapping]] = ..., allocate_block: _Optional[_Union[AllocateBlockOp, _Mapping]] = ..., commit_block: _Optional[_Union[CommitBlockOp, _Mapping]] = ..., finalize_file: _Optional[_Union[FinalizeFileOp, _Mapping]] = ...) -> None: ...

class MkdirOp(_message.Message):
    __slots__ = ("path", "owner")
    PATH_FIELD_NUMBER: _ClassVar[int]
    OWNER_FIELD_NUMBER: _ClassVar[int]
    path: str
    owner: str
    def __init__(self, path: _Optional[str] = ..., owner: _Optional[str] = ...) -> None: ...

class DeleteOp(_message.Message):
    __slots__ = ("path", "recursive")
    PATH_FIELD_NUMBER: _ClassVar[int]
    RECURSIVE_FIELD_NUMBER: _ClassVar[int]
    path: str
    recursive: bool
    def __init__(self, path: _Optional[str] = ..., recursive: bool = ...) -> None: ...

class AllocateBlockOp(_message.Message):
    __slots__ = ("path", "block_index", "block_id", "datanode_ids")
    PATH_FIELD_NUMBER: _ClassVar[int]
    BLOCK_INDEX_FIELD_NUMBER: _ClassVar[int]
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    DATANODE_IDS_FIELD_NUMBER: _ClassVar[int]
    path: str
    block_index: int
    block_id: str
    datanode_ids: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, path: _Optional[str] = ..., block_index: _Optional[int] = ..., block_id: _Optional[str] = ..., datanode_ids: _Optional[_Iterable[str]] = ...) -> None: ...

class CommitBlockOp(_message.Message):
    __slots__ = ("path", "block_id", "size", "checksum_sha256")
    PATH_FIELD_NUMBER: _ClassVar[int]
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    SIZE_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_SHA256_FIELD_NUMBER: _ClassVar[int]
    path: str
    block_id: str
    size: int
    checksum_sha256: str
    def __init__(self, path: _Optional[str] = ..., block_id: _Optional[str] = ..., size: _Optional[int] = ..., checksum_sha256: _Optional[str] = ...) -> None: ...

class FinalizeFileOp(_message.Message):
    __slots__ = ("path", "total_size")
    PATH_FIELD_NUMBER: _ClassVar[int]
    TOTAL_SIZE_FIELD_NUMBER: _ClassVar[int]
    path: str
    total_size: int
    def __init__(self, path: _Optional[str] = ..., total_size: _Optional[int] = ...) -> None: ...

class ReplicateEditLogRequest(_message.Message):
    __slots__ = ("entries",)
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    entries: _containers.RepeatedCompositeFieldContainer[EditLogEntry]
    def __init__(self, entries: _Optional[_Iterable[_Union[EditLogEntry, _Mapping]]] = ...) -> None: ...

class ReplicateEditLogResponse(_message.Message):
    __slots__ = ("last_applied_txid",)
    LAST_APPLIED_TXID_FIELD_NUMBER: _ClassVar[int]
    last_applied_txid: int
    def __init__(self, last_applied_txid: _Optional[int] = ...) -> None: ...

class PingRequest(_message.Message):
    __slots__ = ("node_id", "last_txid")
    NODE_ID_FIELD_NUMBER: _ClassVar[int]
    LAST_TXID_FIELD_NUMBER: _ClassVar[int]
    node_id: str
    last_txid: int
    def __init__(self, node_id: _Optional[str] = ..., last_txid: _Optional[int] = ...) -> None: ...

class PingResponse(_message.Message):
    __slots__ = ("node_id", "is_active", "last_txid")
    NODE_ID_FIELD_NUMBER: _ClassVar[int]
    IS_ACTIVE_FIELD_NUMBER: _ClassVar[int]
    LAST_TXID_FIELD_NUMBER: _ClassVar[int]
    node_id: str
    is_active: bool
    last_txid: int
    def __init__(self, node_id: _Optional[str] = ..., is_active: bool = ..., last_txid: _Optional[int] = ...) -> None: ...
