const RESPONSE_CACHE_CONFIG_GENERATION = "response_cache_config_generation";
const CODEX_IMAGES_DEFAULT_MAIN_MODEL = "codex_images_default_main_model";

export function buildWritableSettingsPayload<
  T extends object,
>(settings: T): Omit<T, typeof RESPONSE_CACHE_CONFIG_GENERATION | typeof CODEX_IMAGES_DEFAULT_MAIN_MODEL> {
  const payload = { ...settings };
  Reflect.deleteProperty(payload, RESPONSE_CACHE_CONFIG_GENERATION);
  Reflect.deleteProperty(payload, CODEX_IMAGES_DEFAULT_MAIN_MODEL);
  return payload;
}
