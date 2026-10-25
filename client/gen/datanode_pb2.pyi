from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Iterable as _Iterable, Mapping as _Mapping, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class DataNodeEndpoint(_message.Message):
    __slots__ = ("id", "host", "port", "rack")
    ID_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    PORT_FIELD_NUMBER: _ClassVar[int]
    RACK_FIELD_NUMBER: _ClassVar[int]
    id: str
    host: str
    port: int
    rack: str
    def __init__(self, id: _Optional[str] = ..., host: _Optional[str] = ..., port: _Optional[int] = ..., rack: _Optional[str] = ...) -> None: ...

class WriteBlockHeader(_message.Message):
    __slots__ = ("block_id", "size", "checksum_sha256", "pipeline")
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    SIZE_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_SHA256_FIELD_NUMBER: _ClassVar[int]
    PIPELINE_FIELD_NUMBER: _ClassVar[int]
    block_id: str
    size: int
    checksum_sha256: str
    pipeline: _containers.RepeatedCompositeFieldContainer[DataNodeEndpoint]
    def __init__(self, block_id: _Optional[str] = ..., size: _Optional[int] = ..., checksum_sha256: _Optional[str] = ..., pipeline: _Optional[_Iterable[_Union[DataNodeEndpoint, _Mapping]]] = ...) -> None: ...

class WriteBlockRequest(_message.Message):
    __slots__ = ("header", "chunk")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    header: WriteBlockHeader
    chunk: bytes
    def __init__(self, header: _Optional[_Union[WriteBlockHeader, _Mapping]] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class WriteBlockResponse(_message.Message):
    __slots__ = ("block_id", "checksum_sha256", "bytes_written", "replica_ids")
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_SHA256_FIELD_NUMBER: _ClassVar[int]
    BYTES_WRITTEN_FIELD_NUMBER: _ClassVar[int]
    REPLICA_IDS_FIELD_NUMBER: _ClassVar[int]
    block_id: str
    checksum_sha256: str
    bytes_written: int
    replica_ids: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, block_id: _Optional[str] = ..., checksum_sha256: _Optional[str] = ..., bytes_written: _Optional[int] = ..., replica_ids: _Optional[_Iterable[str]] = ...) -> None: ...

class ReadBlockRequest(_message.Message):
    __slots__ = ("block_id", "offset", "length")
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    OFFSET_FIELD_NUMBER: _ClassVar[int]
    LENGTH_FIELD_NUMBER: _ClassVar[int]
    block_id: str
    offset: int
    length: int
    def __init__(self, block_id: _Optional[str] = ..., offset: _Optional[int] = ..., length: _Optional[int] = ...) -> None: ...

class ReadBlockResponse(_message.Message):
    __slots__ = ("header", "chunk")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    header: ReadBlockHeader
    chunk: bytes
    def __init__(self, header: _Optional[_Union[ReadBlockHeader, _Mapping]] = ..., chunk: _Optional[bytes] = ...) -> None: ...

class ReadBlockHeader(_message.Message):
    __slots__ = ("block_id", "size", "checksum_sha256")
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    SIZE_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_SHA256_FIELD_NUMBER: _ClassVar[int]
    block_id: str
    size: int
    checksum_sha256: str
    def __init__(self, block_id: _Optional[str] = ..., size: _Optional[int] = ..., checksum_sha256: _Optional[str] = ...) -> None: ...

class DeleteBlockRequest(_message.Message):
    __slots__ = ("block_id",)
    BLOCK_ID_FIELD_NUMBER: _ClassVar[int]
    block_id: str
    def __init__(self, block_id: _Optional[str] = ...) -> None: ...

class DeleteBlockResponse(_message.Message):
    __slots__ = ("deleted",)
    DELETED_FIELD_NUMBER: _ClassVar[int]
    deleted: bool
    def __init__(self, deleted: bool = ...) -> None: ...
