# Operator public capability projection

Interceptor exports its native `interceptor.dev/capability-manifest/v1alpha3`
CapabilityManifest through `interceptor capabilities --environment <directory>`
and the existing native capabilities API. Operator owns
its public TargetCapabilityManifest and ScenarioBundle; the two exports have
distinct schemas and digest recipes.

Follow the [Operator capability contract](../../operator_sandbox/schemas/CAPABILITY_EXPORT_CONTRACT.md)
for the allowlisted native-to-public mapping, reference namespaces and admission
rules. Interceptor's typed manifest digest is unchanged: JSON-marshal the native
manifest with `Digest` empty, retaining the field. Operator retains that source
digest separately from the raw export-file digest and canonical public projection.
No new native endpoint or operation is introduced.

The source digest binds the bundle's authoring provenance. Operator normally
checks whether current capabilities satisfy explicit requirements under current
policy; unrelated native export changes do not invalidate a compatible bundle.
An optional bundle projection digest requires an exact public static match.
Neither mode relaxes Interceptor's campaign, session, attempt or feedback checks.
Operator keeps original source and accepted live bindings separately and supplies
Attack Harness the current execution projection.

The [shared reference-chain fixture](../../operator_sandbox/schemas/fixtures/capability-chain/README.md)
uses the native `capability-delivery.json` golden export. Existing
`TestDeliveryExampleNativeFixtureAndVerification` confirms that the sealed and
verified delivery example produces it. Operator's fixture runner verifies the
public projection and a bundle referencing actual operations, actions and evidence.
These tests qualify serialization/contract mapping, not running Operator or Attack Harness.

The export MUST contain only the specified target/environment testing capabilities
and their provenance. Model-adapter catalogs, authentication methods and upstream
provider configuration MUST NOT be fields in this contract. Provider/authentication
catalog changes MUST NOT contribute fields or values to the manifest or its digest;
release and environment identity changes remain ordinary source provenance.
Both Blueprint versions MUST produce capability-manifest/v1alpha3; delivery-aware
Blueprints additionally set `delivery_schema_profile`. Missing delivery contracts
remain explicit and MUST NOT be invented. Operator and Interceptor MUST be updated
together; administrators MUST re-export saved capabilities and native companions.
Existing retained evidence MUST NOT be rewritten to claim the new schema.
