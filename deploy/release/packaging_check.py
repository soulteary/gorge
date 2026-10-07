"""Validate each candidate's executable, source and healthcheck configuration."""
from manifest import SERVICES

def validate_packaging(info, name, gorge_sha):
    cfg = info.get('Config', {})
    if name not in SERVICES or info.get('Os') != 'linux':
        raise ValueError('Invalid service image')
    if cfg.get('Labels', {}).get('org.opencontainers.image.revision') != gorge_sha:
        raise ValueError('Image source revision does not match the source pair')
    if cfg.get('Entrypoint') != ['gorge-service'] or cfg.get('User') not in ('gorge', '10001'):
        raise ValueError('Image executable or unprivileged user is incorrect')
    if 'GORGE_HEALTHCHECK_PORT='+str(SERVICES[name]) not in cfg.get('Env', []):
        raise ValueError('Image healthcheck port does not match the service')
    if str(SERVICES[name])+'/tcp' not in cfg.get('ExposedPorts', {}):
        raise ValueError('Image exposed port does not match the service')
    if 'GORGE_HEALTHCHECK_PORT' not in ' '.join(cfg.get('Healthcheck', {}).get('Test', [])):
        raise ValueError('Image does not probe the configured health port')
