package contracts

// This file is the machine-readable register of the cross-repository contract:
// the wire field names, HTTP routes and error codes that Phorge's PHP adapters
// spell out as string literals and this service spells out as struct tags,
// route registrations and Go constants. It is the single source of truth the
// drift checker in go/internal/doccheck/contractsync_test.go reads.
//
// Why this exists, in one sentence: the pairs it registers are the ones
// compat/phorge/README.md calls "silent" — rename one on either side and
// nothing errors, a field just quietly goes empty or a document becomes
// unfindable. This register turns "改错不报错" into a CI failure by pinning
// each Go name to the exact phorge-fork file the matching PHP literal lives in,
// so the checker can prove the two spellings still agree.
//
// The correct order for changing a shared name is fixed and load-bearing:
//
//  1. change the Go side (the struct tag / route / constant) AND its entry in
//     this manifest, keeping the two in step;
//  2. run `make docs-check` (or the contractsync test with PHORGE_FORK_DIR set)
//     — it fails and names the exact phorge-fork file to edit;
//  3. change that PHP literal to match, then re-run until it passes.
//
// Going the other way (changing PHP first) leaves this manifest stale and the
// checker green against the wrong truth, which is the drift it exists to catch.
//
// What an empty PHPFiles list means. It is not a category of name ("this one
// never crosses as a literal") — that framing was wrong and hid real drift. It
// is a per-item statement of fact: no quoted PHP literal with that spelling
// exists at a production consumption site in phorge-fork today, for the reason
// spelled out in that item's Note. There are only a handful left and each one
// was re-verified against the tree: the webhook stats fields (the webhook
// client contains no wire-key literal at all, it hands the decoded map
// straight to its caller), the search response
// count/status, taskqueue's dataID (declared as a Lisk property,
// `protected $dataID`, never as a quoted key) and the error codes no PHP
// branch names. Every other item carries files and is verified.
//
// The Go-side half is not taken on faith either: the checker also asserts that
// every WireField below is a real json tag on this package's contract structs
// or a quoted literal in the domain's package, so a field can not be invented
// here without existing on the wire.
//
// The checker also runs in reverse — see ReverseScanFiles below — so a wire key
// that appears in phorge-fork with no counterpart registered here is a failure
// too, not just a silently missing pin.

// ContractItem is one registered name plus the phorge-fork files whose string
// literals must carry it. Name is the exact spelling that crosses the wire (a
// json tag value, a route path, or an error-code string). PHPFiles lists the
// repository-relative paths under the phorge-fork tree where the same spelling
// must appear as a quoted literal; an empty list means no such literal exists
// today and Note says why. Note documents why an item is shaped the way it is.
type ContractItem struct {
	Name     string
	PHPFiles []string
	Note     string
}

// DomainContract groups one domain's registered routes, error codes and wire
// field names. Domain is both the label used in failure messages and the key
// the checker uses to find the Go package whose http.go registers the routes
// and whose files declare the error-code constants.
type DomainContract struct {
	Domain     string
	Routes     []ContractItem
	ErrorCodes []ContractItem
	WireFields []ContractItem
}

// phorge-fork paths, kept as constants so a file move is one edit rather than
// dozens. They are repository-relative: the checker joins them onto whatever
// PHORGE_FORK_DIR points at.
const (
	phpMetaMail           = "src/applications/metamta/storage/PhabricatorMetaMTAMail.php"
	phpMailerClient       = "src/infrastructure/cluster/PhabricatorGorgeMailerClient.php"
	phpSearchClient       = "src/infrastructure/cluster/PhabricatorGorgeSearchClient.php"
	phpTaskQueueClnt      = "src/infrastructure/cluster/PhabricatorGorgeTaskQueueClient.php"
	phpWebhookClient      = "src/infrastructure/cluster/PhabricatorGorgeWebhookClient.php"
	phpDBClient           = "src/infrastructure/cluster/PhabricatorGorgeDBClient.php"
	phpDatabaseRef        = "src/infrastructure/cluster/PhabricatorDatabaseRef.php"
	phpSchemaQuery        = "src/applications/config/schema/PhabricatorConfigSchemaQuery.php"
	phpFieldTypeConst     = "src/applications/search/constants/PhabricatorSearchDocumentFieldType.php"
	phpRelationConst      = "src/applications/search/constants/PhabricatorSearchRelationship.php"
	phpDocumentSerializer = "src/applications/search/index/PhabricatorSearchDocumentSerializer.php"
	phpFulltextEngine     = "src/applications/search/fulltextstorage/PhabricatorGorgeFulltextStorageEngine.php"
	phpHeraldRequest      = "src/applications/herald/storage/HeraldWebhookRequest.php"
	phpHeraldWorker       = "src/applications/herald/worker/HeraldWebhookWorker.php"
	phpWorkerTask         = "src/infrastructure/daemon/workers/storage/PhabricatorWorkerTask.php"
	phpWorkerExecute      = "src/infrastructure/daemon/workers/conduit/PhabricatorWorkerExecuteConduitAPIMethod.php"
	phpFeedPublisher      = "src/applications/feed/worker/FeedPublisherWorker.php"
)

// Manifest returns the whole cross-repository contract register. It is a
// function rather than a package var so callers (the checker, and any future
// tooling) always get a fresh copy they can not accidentally mutate for others.
func Manifest() []DomainContract {
	return []DomainContract{
		imageContract(),
		mailerContract(),
		searchContract(),
		webhookContract(),
		taskqueueContract(),
		dbapiContract(),
	}
}

// mailerContract registers §6 of compat/phorge/README.md: the two routes, the
// three error codes, and the camelCase message field names. The request envelope
// key and every field inside it appear in PhabricatorGorgeMailerClient
// (sendMessage() builds and validates the envelope, serializeMessage() the body).
// SendResult fields are checked by the PHP client before acceptance is recorded.
func mailerContract() DomainContract {
	return DomainContract{
		Domain: "mailer",
		Routes: []ContractItem{
			{Name: "/api/mailer/delivery", Note: "Authenticated read-only operational inspection; no PHP caller."},
			{Name: "/api/mailer/delivery-capabilities", Note: "Native Go worker readiness negotiates mail ledger and recovery support."},
			{Name: "/api/mailer/prepare", Note: "Native Go preparation handler persists PHP-exported snapshot; PHP does not call this route."},
			{Name: "/api/mailer/execute", Note: "Native Go submission handler references a durable snapshot."},
			{Name: "/api/mailer/deliver", Note: "Authenticated immutable delivery API for Go clients."},
			{Name: "/api/mailer/cancel", Note: "Native Go policy gate cancels pending delivery."},
			{Name: "/api/mailer/send", PHPFiles: []string{phpMailerClient}},
			{Name: "/api/mailer/mailers", PHPFiles: []string{phpMailerClient}},
		},
		ErrorCodes: []ContractItem{
			{
				Name:     "ERR_PERMANENT_FAILURE",
				PHPFiles: []string{phpMailerClient},
				Note:     "§6.2 explicit permanent message rejection stops automatic resubmission.",
			},
			{
				Name: "ERR_SEND_FAILED",
				Note: "§6.2 proven nonacceptance; PHP defaults to retry for this code without naming it as a literal.",
			},
			{Name: "ERR_OUTCOME_UNKNOWN", PHPFiles: []string{phpMailerClient}, Note: "§6.2 uncertain submission is retained for reconciliation and must not be automatically re-sent."},
		},
		WireFields: []ContractItem{
			{Name: "error", PHPFiles: []string{phpMailerClient}, Note: "Shared httpx response envelope; PHP validates it before classifying delivery failure."},
			{Name: "code", PHPFiles: []string{phpMailerClient}, Note: "Shared httpx error discriminator; PHP selects permanent, unknown or safe-retry handling."},
			{Name: "deliveryID", PHPFiles: []string{phpMetaMail}},
			{Name: "mailID", PHPFiles: []string{phpMetaMail}},
			{Name: "deadline", PHPFiles: []string{phpMetaMail}},
			{Name: "schemaVersion", PHPFiles: []string{phpMetaMail}},
			{Name: "mailerURI", PHPFiles: []string{phpMetaMail}},
			{Name: "adapterKey", PHPFiles: []string{phpMetaMail}},
			{Name: "revision", PHPFiles: []string{phpMetaMail}},
			{Name: "state", PHPFiles: []string{phpMetaMail}},
			{Name: "allowSend", Note: "Execution-time Go policy gate, excluded from immutable hash; PHP only prepares domain content."},

			{Name: "message", PHPFiles: []string{phpMailerClient}, Note: "§6.1 the outermost request-envelope key; sendMessage() wraps serializeMessage() under it."},
			{Name: "mailerKeys", PHPFiles: []string{phpMailerClient}, Note: "§6.1 the other envelope key; restricts delivery to named backends."},
			{Name: "from", PHPFiles: []string{phpMailerClient}},
			{Name: "replyTo", PHPFiles: []string{phpMailerClient}},
			{Name: "to", PHPFiles: []string{phpMailerClient}},
			{Name: "cc", PHPFiles: []string{phpMailerClient}},
			{Name: "subject", PHPFiles: []string{phpMailerClient}},
			{Name: "textBody", PHPFiles: []string{phpMailerClient}},
			{Name: "htmlBody", PHPFiles: []string{phpMailerClient}},
			{Name: "headers", PHPFiles: []string{phpMailerClient}},
			{Name: "attachments", PHPFiles: []string{phpMailerClient}},
			{Name: "name", PHPFiles: []string{phpMailerClient}, Note: "address display name and header name."},
			{Name: "address", PHPFiles: []string{phpMailerClient}},
			{Name: "value", PHPFiles: []string{phpMailerClient}, Note: "header value."},
			{Name: "filename", PHPFiles: []string{phpMailerClient}},
			{Name: "mimeType", PHPFiles: []string{phpMailerClient}},
			{Name: "data", PHPFiles: []string{phpMailerClient}, Note: "attachment payload, base64 at this layer."},
			{Name: "mailerKey", PHPFiles: []string{phpMailerClient}, Note: "SendResult; PHP validates the acceptance receipt."},
			{Name: "messageId", PHPFiles: []string{phpMailerClient}, Note: "SendResult; optional provider receipt validated when present."},
		},
	}
}

// searchContract registers §7 of compat/phorge/README.md. The seven routes and
// the four envelope field names appear in PhabricatorGorgeSearchClient; the 16
// four-character names are the values of the two PHP constant classes and are
// checked against those files verbatim (§7.3).
//
// The §7.2 document, query and stats field names are checked against
// the shared PhabricatorSearchDocumentSerializer and
// PhabricatorGorgeFulltextStorageEngine: newDocumentSpec() and newQuerySpec()
// build the request arrays key by key and
// getIndexStats() names the five stats keys in its label map. An earlier
// version of this file claimed they were "flattened and read generically" and
// left them unpinned; that was false and left §7.2 — the half of the section
// the README calls silent — with no guard at all.
func searchContract() DomainContract {
	return DomainContract{
		Domain: "search",
		Routes: []ContractItem{
			{Name: "/api/search/index", PHPFiles: []string{phpSearchClient}},
			{Name: "/api/search/query", PHPFiles: []string{phpSearchClient}},
			{Name: "/api/search/init", PHPFiles: []string{phpSearchClient}},
			{Name: "/api/search/exists", PHPFiles: []string{phpSearchClient}},
			{Name: "/api/search/stats", PHPFiles: []string{phpSearchClient}},
			{Name: "/api/search/sane", PHPFiles: []string{phpSearchClient}},
			{Name: "/api/search/backends", PHPFiles: []string{phpSearchClient}},
		},
		ErrorCodes: []ContractItem{
			// §附录: the five search codes have no PHP consumer that branches on
			// them (PhabricatorGorgeSearchClient does not override
			// newServiceErrorException), so they are registered for Go-side
			// coverage only and carry no PHPFiles.
			{Name: "ERR_INDEX_FAILED", Note: "no PHP branch consumes it; collapses into the generic exception."},
			{Name: "ERR_SEARCH_FAILED", Note: "no PHP branch consumes it."},
			{Name: "ERR_INIT_FAILED", Note: "no PHP branch consumes it."},
			{Name: "ERR_CHECK_FAILED", Note: "no PHP branch consumes it."},
			{Name: "ERR_STATS_FAILED", Note: "no PHP branch consumes it."},
		},
		WireFields: []ContractItem{
			// §7.3 the 16 four-character names, pinned to the PHP constant
			// classes. These are the loudest-silence pair in the whole
			// contract: a mismatch makes documents unfindable with no error.
			{Name: "titl", PHPFiles: []string{phpFieldTypeConst}, Note: "§7.3 FIELD_TITLE."},
			{Name: "body", PHPFiles: []string{phpFieldTypeConst}, Note: "§7.3 FIELD_BODY."},
			{Name: "cmnt", PHPFiles: []string{phpFieldTypeConst}, Note: "§7.3 FIELD_COMMENT."},
			{Name: "full", PHPFiles: []string{phpFieldTypeConst}, Note: "§7.3 FIELD_ALL."},
			{Name: "core", PHPFiles: []string{phpFieldTypeConst}, Note: "§7.3 FIELD_CORE."},
			{Name: "auth", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_AUTHOR."},
			{Name: "book", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_BOOK."},
			{Name: "revw", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_REVIEWER."},
			{Name: "subs", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_SUBSCRIBER."},
			{Name: "comm", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_COMMENTER."},
			{Name: "ownr", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_OWNER."},
			{Name: "proj", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_PROJECT."},
			{Name: "repo", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_REPOSITORY."},
			{Name: "open", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_OPEN (status marker)."},
			{Name: "clos", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_CLOSED (status marker)."},
			{Name: "unow", PHPFiles: []string{phpRelationConst}, Note: "§7.3 RELATIONSHIP_UNOWNED (status marker)."},
			// §7.2 document keys, shared by adapters and exporters via the serializer.
			{Name: "phid", PHPFiles: []string{phpDocumentSerializer}, Note: "§7.2 document identity."},
			{Name: "type", PHPFiles: []string{phpDocumentSerializer}, Note: "§7.2 document type."},
			{Name: "title", PHPFiles: []string{phpDocumentSerializer}},
			{Name: "dateCreated", PHPFiles: []string{phpDocumentSerializer}},
			{Name: "dateModified", PHPFiles: []string{phpDocumentSerializer}},
			{Name: "fields", PHPFiles: []string{phpDocumentSerializer}, Note: "§7.2 the field-tuple list; a document may carry several corpora per field name."},
			{Name: "relationships", PHPFiles: []string{phpDocumentSerializer}},
			{Name: "name", PHPFiles: []string{phpDocumentSerializer}, Note: "§7.2 the four-character field/relationship name inside a tuple."},
			{Name: "corpus", PHPFiles: []string{phpDocumentSerializer}},
			{Name: "aux", PHPFiles: []string{phpDocumentSerializer}},
			{Name: "relatedPHID", PHPFiles: []string{phpDocumentSerializer}, Note: "§7.2 the README's own example of a silent rename: collapse it into phid and every relationship goes empty."},
			{Name: "rtype", PHPFiles: []string{phpDocumentSerializer}},
			{Name: "timestamp", PHPFiles: []string{phpDocumentSerializer}},
			// §7.2 query keys, built by name in newQuerySpec().
			{Name: "query", PHPFiles: []string{phpFulltextEngine}},
			{Name: "types", PHPFiles: []string{phpFulltextEngine}},
			{Name: "authorPHIDs", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 one of the six list parameters copied through by name."},
			{Name: "ownerPHIDs", PHPFiles: []string{phpFulltextEngine}},
			{Name: "subscriberPHIDs", PHPFiles: []string{phpFulltextEngine}},
			{Name: "projectPHIDs", PHPFiles: []string{phpFulltextEngine}},
			{Name: "repositoryPHIDs", PHPFiles: []string{phpFulltextEngine}},
			{Name: "statuses", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 carries the open/clos relationship constants, not PHIDs."},
			{Name: "withAnyOwner", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 drops to \"no filter\" if lost, and \"no filter\" looks like a plausible result set."},
			{Name: "withUnowned", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 same trap as withAnyOwner."},
			{Name: "exclude", PHPFiles: []string{phpFulltextEngine}},
			{Name: "offset", PHPFiles: []string{phpFulltextEngine}},
			{Name: "limit", PHPFiles: []string{phpFulltextEngine}},
			// §7.2 stats keys, named in getIndexStats()'s label map. IndexStats
			// is an open map on the Go side, so these prove out against the
			// backend sources rather than a json tag.
			{Name: "queries", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 stats; Elasticsearch only."},
			{Name: "documents", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 stats; both backends."},
			{Name: "deleted", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 stats; Elasticsearch only."},
			{Name: "storage_bytes", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 the one snake_case name on the wire; renaming it blanks the cluster panel's storage column."},
			{Name: "indexing", PHPFiles: []string{phpFulltextEngine}, Note: "§7.2 stats; Meilisearch only."},
			// §7.2 response fields the client names when it unwraps envelopes.
			{Name: "phids", PHPFiles: []string{phpSearchClient}, Note: "SearchResults; read by name in executeQuery()."},
			{Name: "docTypes", PHPFiles: []string{phpSearchClient}, Note: "DocTypesRequest; built by name in initIndex()/indexIsSane()."},
			{Name: "exists", PHPFiles: []string{phpSearchClient}, Note: "IndexPresence; read by name in indexExists()."},
			{Name: "sane", PHPFiles: []string{phpSearchClient}, Note: "IndexSanity; read by name in indexIsSane()."},
			{Name: "count", Note: "SearchResults; the client returns the decoded data section and only names phids, so no literal exists to compare."},
			{Name: "status", Note: "IndexResult; the engine ignores the index response body entirely, so this key is never named on the PHP side."},
		},
	}
}

// webhookContract registers §9. Diagnostic routes remain PHP adapters.
// Gorge now constructs the receiver payload exclusively; byte-exact fixtures
// protect its shape. PHP still produces the action flags stored in the request
// row, so those flags are pinned to the request DAO rather than the retired
// payload builder. SQL column writeback is outside this JSON-name register.
func webhookContract() DomainContract {
	return DomainContract{
		Domain: "webhook",
		Routes: []ContractItem{
			{Name: "/api/webhook/stats", PHPFiles: []string{phpWebhookClient}},
			{Name: "/api/webhook/hooks", PHPFiles: []string{phpWebhookClient}},
		},
		WireFields: []ContractItem{
			// §9.1 payload keys. Key order is part of the contract too, but
			// that is byte-exactness and belongs to TestPayloadIsByteExact;
			// this only pins the spellings.
			{Name: "object", Note: "Go-owned receiver payload root; PHP no longer constructs this document."},
			{Name: "type", Note: "Go derives object type from the PHID; no PHP payload builder remains."},
			{Name: "phid", Note: "Go emits object, trigger and transaction identity; PHP stores PHIDs under different SQL/property names."},
			{Name: "triggers", Note: "Go converts stored triggerPHIDs to receiver objects."},
			{Name: "action", Note: "Go builds the receiver action envelope from stored request flags."},
			{Name: "test", PHPFiles: []string{phpHeraldRequest}},
			{Name: "silent", PHPFiles: []string{phpHeraldRequest}, Note: "§9.9 per-request, not the global phabricator.silent setting."},
			{Name: "secure", PHPFiles: []string{phpHeraldRequest}},
			{Name: "epoch", Note: "Go emits request dateCreated as receiver epoch; no PHP payload builder remains."},
			{Name: "transactions", Note: "Go converts stored transactionPHIDs to receiver objects."},
			// §9.8 stats fields.
			{Name: "queuedCount", Note: "DeliveryStats; the client names no key at all, so there is no literal to compare."},
			{Name: "sentCount", Note: "DeliveryStats; same as queuedCount."},
			{Name: "failedCount", Note: "DeliveryStats; same as queuedCount."},
			{Name: "activeWebhooks", Note: "DeliveryStats; same as queuedCount."},
			{Name: "total", Note: "HookSummary; same as queuedCount."},
		},
	}
}

// taskqueueContract registers §10. The routes and the request field names
// appear as literals in PhabricatorGorgeTaskQueueClient. Four of the §10.1
// column names are also literals — PhabricatorWorkerTask declares them in its
// Lisk CONFIG_COLUMN_SCHEMA map, which is the schema's own spelling — so they
// are pinned there. Only dataID is not: it exists on that class as a plain
// property declaration (`protected $dataID;`), never as a quoted key, and the
// checker compares quoted literals only. An earlier version of this file said
// all five were "Lisk-mapped, not named in this client" and left all five
// unpinned, which was true of the client and false of the schema.
func taskqueueContract() DomainContract {
	return DomainContract{
		Domain: "taskqueue",
		Routes: []ContractItem{
			{Name: "/api/queue/meta", Note: "Go worker negotiates execution capabilities; no PHP client consumes this route."},
			{Name: "/api/queue/finalize", Note: "Go worker atomically commits PHP-exported followups; PHP does not call it."},
			{Name: "/api/queue/renew", Note: "Go worker manages lease renewal; no PHP route literal."},
			{Name: "/api/queue/resolve", Note: "Go worker reports fenced failures/yields; no PHP route literal."},
			{Name: "/api/queue/enqueue-event", Note: "Go outbox relay consumes this route; PHP writes the source event locally."},
			{Name: "/api/queue/enqueue", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "/api/queue/lease", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "/api/queue/complete", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "/api/queue/fail", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "/api/queue/yield", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "/api/queue/cancel", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "/api/queue/awaken", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "/api/queue/stats", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "/api/queue/tasks", PHPFiles: []string{phpTaskQueueClnt}},
		},
		ErrorCodes: []ContractItem{
			{Name: "ERR_LEASE_CONFLICT", Note: "Go worker stops reporting when ownership is lost; no PHP branch consumes the code."},
			{Name: "ERR_EVENT_CONFLICT", Note: "Go relay keeps a conflicting event pending; no PHP branch consumes the code."},
		},
		WireFields: []ContractItem{
			{Name: "eventID", PHPFiles: []string{phpFeedPublisher}, Note: "Feed transaction outbox event identity, preserved into the queue inbox."},
			{Name: "task", PHPFiles: []string{phpFeedPublisher}, Note: "Outbox event wraps a typed queue request."},
			{Name: "executionVersion", PHPFiles: []string{phpWorkerExecute}},
			{Name: "followups", PHPFiles: []string{phpWorkerExecute}},
			{Name: "leaseOutcomes", Note: "Queue capability consumed by Go worker; no PHP literal."},
			{Name: "outcome", Note: "Fenced retry/failure/yield classification consumed within Go; PHP reports result instead."},
			// Request-body field names the client builds by name.
			{Name: "taskClass", PHPFiles: []string{phpTaskQueueClnt}, Note: "§10.1 worker_activetask.taskClass."},
			{Name: "data", PHPFiles: []string{phpTaskQueueClnt}, Note: "§10.1 serialized task data."},
			{Name: "priority", PHPFiles: []string{phpTaskQueueClnt}, Note: "§10.3 priority band."},
			{Name: "objectPHID", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "containerPHID", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "delayUntil", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "taskID", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "duration", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "permanent", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "retryWait", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "taskIDs", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "limit", PHPFiles: []string{phpTaskQueueClnt}},
			{Name: "offset", PHPFiles: []string{phpTaskQueueClnt}, Note: "§10 the /tasks query parameter; read with c.Query on the Go side, so it is a literal there rather than a json tag."},
			// §10.1 column names, pinned to the Lisk schema declaration.
			{Name: "leaseOwner", PHPFiles: []string{phpWorkerTask}, Note: "§10.1/§10.2 lease holder column."},
			{Name: "leaseExpires", PHPFiles: []string{phpWorkerTask}, Note: "§10.2 lease-expiry column; the preemption floor."},
			{Name: "failureCount", PHPFiles: []string{phpWorkerTask}, Note: "§10.1 column."},
			{Name: "failureTime", PHPFiles: []string{phpWorkerTask}, Note: "§10.1 column."},
			{Name: "dataID", Note: "§10.1 foreign key into worker_taskdata. PhabricatorWorkerTask declares it as a property rather than a CONFIG_COLUMN_SCHEMA key, so no quoted literal for it exists on that class to compare against."},
		},
	}
}

// dbapiContract registers §11. Every field here has a production consumption
// site in the PHP adapter, PhabricatorDatabaseRef, or PhabricatorConfigSchemaQuery
// (fixtures under __tests__/ are intentionally not used as the source of truth).
// The three domain error codes are read for their code text only — the search
// story of §附录 — so ERR_READONLY, which no read route reaches today, is still
// registered so it can not be quietly dropped.
//
// One near miss worth recording so nobody "completes" the list with it:
// PhabricatorDatabaseRef also spells 'user', but as an outbound connection
// parameter it builds for its own MySQL handshake, not as a key it reads out of
// a gorge response. ServerRef.User happens to share the spelling, which makes
// it look like a hit; pinning it would tie a gorge contract field to a literal
// that has nothing to do with the wire.
func dbapiContract() DomainContract {
	return DomainContract{
		Domain: "dbapi",
		Routes: []ContractItem{
			{Name: "/api/db/servers", PHPFiles: []string{phpDBClient}},
			{Name: "/api/db/schema-diff", PHPFiles: []string{phpDBClient}},
			{Name: "/api/db/schema-issues", PHPFiles: []string{phpDBClient}},
			{Name: "/api/db/setup-issues", PHPFiles: []string{phpDBClient}},
			{Name: "/api/db/charset-info", PHPFiles: []string{phpDBClient}},
			{Name: "/api/db/migrations/status", PHPFiles: []string{phpDBClient}},
			{Name: "/api/db/meta", PHPFiles: []string{phpDBClient}},
		},
		ErrorCodes: []ContractItem{
			{Name: "ERR_DB_UNREACHABLE", Note: "§11.3 503; code text only, no PHP branch names the literal."},
			{Name: "ERR_READONLY", Note: "§11.3 409; defined but no read route reaches it — kept so it is not dropped."},
			{Name: "ERR_DB_ACCESS_DENIED", Note: "§11.3 403; code text only."},
		},
		WireFields: []ContractItem{
			// ServerRef, read by PhabricatorDatabaseRef::newRefsFromGorge and
			// the health path below it.
			{Name: "refKey", PHPFiles: []string{phpDatabaseRef, phpSchemaQuery}, Note: "§11.1 host:port identity; both the ref builder and the schema query match on it."},
			{Name: "host", PHPFiles: []string{phpDatabaseRef}, Note: "§11.1 read when rebuilding a ref the configuration did not already describe."},
			{Name: "port", PHPFiles: []string{phpDatabaseRef}},
			{Name: "connectionStatus", PHPFiles: []string{phpDatabaseRef}},
			{Name: "connectionMessage", PHPFiles: []string{phpDatabaseRef}},
			{Name: "connectionLatencySec", PHPFiles: []string{phpDatabaseRef}},
			{Name: "replicationStatus", PHPFiles: []string{phpDatabaseRef}},
			{Name: "replicaMessage", PHPFiles: []string{phpDatabaseRef}},
			{Name: "secondsBehindMaster", PHPFiles: []string{phpDatabaseRef}},
			// SchemaNode tree keys, read by PhabricatorConfigSchemaQuery's
			// newServerSchemaFromGorgeNode(). Its own docblock enumerates them,
			// which is a good sign it will notice a rename — but only if
			// somebody reads the docblock, which is what this pins instead.
			{Name: "databases", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 the schema-diff request parameter; read with c.Query on the Go side."},
			{Name: "children", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 the server -> database -> table -> column nesting key."},
			{Name: "databaseName", PHPFiles: []string{phpSchemaQuery}},
			{Name: "tableName", PHPFiles: []string{phpSchemaQuery}},
			{Name: "columnName", PHPFiles: []string{phpSchemaQuery}},
			{Name: "characterSet", PHPFiles: []string{phpSchemaQuery}},
			{Name: "collation", PHPFiles: []string{phpSchemaQuery}},
			{Name: "engine", PHPFiles: []string{phpSchemaQuery}},
			{Name: "columnType", PHPFiles: []string{phpSchemaQuery}},
			{Name: "nullable", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 read only when present, so a rename reads as \"not reported\" rather than false."},
			{Name: "autoIncrement", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 same present-or-absent read as nullable."},
			{Name: "accessDenied", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 distinguishes a restricted database from a missing one."},
			{Name: "keys", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 the index list hanging off a table node."},
			{Name: "columnNames", PHPFiles: []string{phpSchemaQuery}},
			{Name: "unique", PHPFiles: []string{phpSchemaQuery}},
			{Name: "indexType", PHPFiles: []string{phpSchemaQuery}},
			// CharsetInfo, read by buildExpectedSchemaFromCharset().
			{Name: "charsetDefault", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 falls back to utf8mb4 when absent, so a rename degrades silently to the default."},
			{Name: "collateText", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 same silent fallback."},
			{Name: "collateSort", PHPFiles: []string{phpSchemaQuery}, Note: "§11.1 same silent fallback."},
			// SetupIssue / MigrationStatus / Capabilities, read by the adapter.
			{Name: "issueKey", PHPFiles: []string{phpDBClient}, Note: "§11.1 read in newSetupIssueFromRow()."},
			{Name: "name", PHPFiles: []string{phpDBClient, phpSchemaQuery}, Note: "§11.1 the setup issue's display name, and the key schema's index name."},
			{Name: "summary", PHPFiles: []string{phpDBClient}, Note: "§11.1 read in newSetupIssueFromRow()."},
			{Name: "message", PHPFiles: []string{phpDBClient}, Note: "§11.1 read in newSetupIssueFromRow()."},
			{Name: "isFatal", PHPFiles: []string{phpDBClient}, Note: "§11.2 the load-bearing field; decides block-vs-warn."},
			{Name: "initialized", PHPFiles: []string{phpDBClient}, Note: "§11.4 read in missingPatchesForStatus()."},
			{Name: "appliedPatches", PHPFiles: []string{phpDBClient}, Note: "§11.4 observed patch set."},
			{Name: "contractVersion", PHPFiles: []string{phpDBClient}, Note: "§11 handshake key."},
			{Name: "namespace", PHPFiles: []string{phpDBClient}, Note: "§11 handshake key."},
			{Name: "capabilities", PHPFiles: []string{phpDBClient}, Note: "§11 handshake key; the adapter checks the advertised list before using a route."},
		},
	}
}

// ReverseScanFile is one phorge-fork file the checker reads in the opposite
// direction: instead of asking "does the manifest's name appear here", it asks
// "does every wire key here appear in the manifest". Path is repository
// relative under PHORGE_FORK_DIR. NonContractKeys lists the keys in this file
// that look like wire keys but are not, each with its reason; Note records why
// the file is in scope at all.
type ReverseScanFile struct {
	Path            string
	NonContractKeys []string
	Note            string
}

// ReverseScanFiles returns the files the reverse check reads, which is the
// other half of what the drift plan asks for: a name that exists on the PHP
// side with no Go counterpart is drift too, and the forward check can not see
// it — a pin nobody adds is a pin nobody misses.
//
// The hard part is scope, because a PHP file is full of string literals that
// have nothing to do with this contract. Three rules keep the false-positive
// rate at zero without whitelisting the interesting cases away:
//
//  1. Only files that actually sit on the wire boundary are scanned. That is
//     the list below: the five gorge service clients, the fulltext storage
//     engine, the herald worker that builds the §9.1 payload, the two PHP
//     consumers of the db-api responses, and the Lisk schema §10.1 pins to.
//     Everything else in phorge-fork is out of scope by construction.
//
//  2. Within those files, only three syntactic positions count as a wire key:
//     `idx($x, 'k')` (reading a decoded response), `'k' => …` (building a
//     request body or declaring a schema column) and `$x['k']` (either). A key
//     that never appears in one of those is not being used as a wire key.
//
//  3. Only lowerCamelCase spellings are considered. Every name in this contract
//     is lowerCamelCase by §7.2's and §11.1's own rule, so anything else can
//     not be one. This is what keeps INFORMATION_SCHEMA column names
//     (TABLE_SCHEMA, Non_unique, Column_name …) out without naming them one by
//     one — there are dozens and they would swamp the exception list. It also
//     skips search's storage_bytes, which is registered and forward-checked
//     anyway; the reverse direction gives up one snake_case name to keep the
//     rule stateable in a sentence.
//
// What survives all three and still is not a contract key is listed per file
// below, with the reason. The list is short enough to audit by hand, which is
// the point: if it starts growing, the scope above is wrong.
//
// One limitation, recorded rather than papered over: a key passes if it is
// registered anywhere in the manifest, not necessarily against this file. So
// this catches "PHP has a name Go has never heard of", not "PHP reads this key
// here and the manifest pins it somewhere else". The second one is a coverage
// gap rather than drift, and treating it as an error would force every shared
// spelling to be cross-listed against every file that touches it.
func ReverseScanFiles() []ReverseScanFile {
	return []ReverseScanFile{
		{
			Path: phpMailerClient,
			Note: "§6.1; serializeMessage() and sendMessage() are pure wire construction.",
		},
		{
			Path: phpSearchClient,
			Note: "§7.2 envelope keys.",
		},
		{
			Path: phpTaskQueueClnt,
			Note: "§10 request bodies.",
		},
		{
			Path: phpWebhookClient,
			Note: "§9.8; it names no key today, which is exactly the claim the webhook stats items make. If a key ever appears here, one of those items stops being true and this check says so.",
		},
		{
			Path: phpDocumentSerializer,
			Note: "Shared document serializer used by storage and batch export.",
		},
		{
			Path: phpFulltextEngine,
			Note: "§7.2 query and stats keys.",
		},
		{
			Path:            phpHeraldWorker,
			NonContractKeys: []string{"webhookRequestPHID"},
			Note:            "Retired delivery compatibility sink. webhookRequestPHID identifies its task; receiver payload construction belongs to Go.",
		},
		{
			Path:            phpDBClient,
			NonContractKeys: []string{"checked", "usable", "code", "detail"},
			Note:            "§11.1/§11.2 response reads. checked/usable are the adapter's in-process availability cache; code/detail are PhabricatorSetupIssue's own constructor keys, which the adapter fills in from the gorge fields it just read.",
		},
		{
			Path: phpDatabaseRef,
			NonContractKeys: []string{
				"user", "pass", "database", "persistent", "timeout", "retries",
				"color", "icon", "label",
			},
			Note: "§11.1 ServerRef reads. The first six are outbound MySQL connection-spec keys this class builds for its own handshake — 'user' is the near miss called out on dbapiContract — and color/icon/label are the UI status table.",
		},
		{
			Path: phpSchemaQuery,
			Note: "§11.1 SchemaNode and CharsetInfo reads. The native non-gorge path in the same file reads INFORMATION_SCHEMA columns by name, but every one of those is upper case or underscored, so rule 3 drops them.",
		},
		{
			Path:            phpWorkerTask,
			NonContractKeys: []string{"columns"},
			Note:            "§10.1 the Lisk column schema. 'columns' is the key schema's own structural key, not a column name.",
		},
	}
}

// Image computation is binary; metadata and protocol pins remain JSON.
func imageContract() DomainContract {
	php := "src/infrastructure/cluster/PhabricatorGorgeImageClient.php"
	check := "src/applications/config/check/PhabricatorGorgeImageSetupCheck.php"
	file := "src/applications/files/storage/PhabricatorFile.php"
	return DomainContract{Domain: "image", Routes: []ContractItem{
		{Name: "/api/image/stats", Note: "Go computation-cache observability; no PHP adapter consumes this route."},
		{Name: "/api/image/transform", PHPFiles: []string{php}},
		{Name: "/api/image/probe", PHPFiles: []string{php}},
		{Name: "/api/image/capabilities", PHPFiles: []string{php}},
	}, WireFields: []ContractItem{
		{Name: "width", PHPFiles: []string{file}}, {Name: "height", PHPFiles: []string{file}},
		{Name: "mimeType", Note: "Probe metadata; PHP validates returned image bytes independently."},
		{Name: "frames", Note: "Probe reports frame count; no PHP literal consumer yet."},
		{Name: "animationStatus", Note: "Probe reports static/animated; PHP specifies policy in request."},
		{Name: "protocolVersion", PHPFiles: []string{check}}, {Name: "recipeRevision", PHPFiles: []string{check}},
		{Name: "recipes", PHPFiles: []string{check}},
	}}
}
