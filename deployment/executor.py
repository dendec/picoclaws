import json
import os
import boto3
import logging
import shlex
import time
import sys
import io
import importlib.util
import tempfile
from contextlib import redirect_stdout, redirect_stderr

# Configure structured JSON logging
class JsonFormatter(logging.Formatter):
    def format(self, record):
        log_record = {
            "level": record.levelname.lower(),
            "time": int(time.time()),
            "message": record.getMessage(),
            "component": "task-executor",
        }
        # Add correlation IDs if present
        if hasattr(record, "task_id"):
            log_record["task_id"] = record.task_id
        if hasattr(record, "request_id"):
            log_record["request_id"] = record.request_id
        if hasattr(record, "chat_id"):
            log_record["chat_id"] = record.chat_id
        if record.exc_info:
            log_record["exception"] = self.formatException(record.exc_info)
        return json.dumps(log_record)

logger = logging.getLogger("executor")
handler = logging.StreamHandler()
handler.setFormatter(JsonFormatter())
logger.addHandler(handler)
logger.setLevel(logging.INFO)

sqs = boto3.client('sqs')
QUEUE_URL = os.environ.get('QUEUE_URL')
RESULT_QUEUE_URL = os.environ.get('RESULT_QUEUE_URL')
TASK_ROOT = os.environ.get('LAMBDA_TASK_ROOT', '/var/task')

# Ensure libraries and skills are in sys.path
if TASK_ROOT not in sys.path:
    sys.path.insert(0, TASK_ROOT)
if f"{TASK_ROOT}/lib" not in sys.path:
    sys.path.insert(0, f"{TASK_ROOT}/lib")

def run_engine(entrypoint_rel_path, args, extra_logs):
    """
    Executes a skill engine script in-process.
    entrypoint_rel_path: path relative to TASK_ROOT
    args: list of command line arguments
    extra_logs: dict with logging context (task_id, etc.)
    """
    entrypoint_abs_path = os.path.join(TASK_ROOT, entrypoint_rel_path)
    if not os.path.exists(entrypoint_abs_path):
        return None, f"Entrypoint script not found: {entrypoint_rel_path}"

    # Use unique module name to avoid collisions (e.g. skills.draw.main)
    module_name = entrypoint_rel_path.replace("/", ".").replace(".py", "")
    engine_dir = os.path.dirname(entrypoint_abs_path)
    
    with tempfile.TemporaryDirectory() as tmp_dir:
        orig_argv = sys.argv
        sys.argv = [entrypoint_abs_path] + args
        
        stdout = io.StringIO()
        stderr = io.StringIO()
        
        # Add engine directory to sys.path temporarily for internal imports (from common import ...)
        added_to_path = False
        if engine_dir not in sys.path:
            sys.path.insert(0, engine_dir)
            added_to_path = True
            
        try:
            # Dynamic load/cache module
            spec = importlib.util.spec_from_file_location(module_name, entrypoint_abs_path)
            if module_name in sys.modules:
                module = sys.modules[module_name]
            else:
                module = importlib.util.module_from_spec(spec)
                sys.modules[module_name] = module
                spec.loader.exec_module(module)
            
            with redirect_stdout(stdout), redirect_stderr(stderr):
                try:
                    if hasattr(module, 'main'):
                        module.main()
                    else:
                        # Fallback for scripts without main()
                        exec(open(entrypoint_abs_path).read(), module.__dict__)
                except SystemExit as e:
                    if (e.code or 0) != 0:
                        out = stdout.getvalue()
                        err = stderr.getvalue()
                        combined = err if err else out
                        return None, f"Exited with code {e.code}: {combined}"

            return stdout.getvalue(), stderr.getvalue()
        except Exception as e:
            logger.error(f"In-process execution failed: {e}", extra=extra_logs, exc_info=True)
            # Include captured output in the exception message if possible
            return None, f"{str(e)}\nSTDOUT: {stdout.getvalue()}\nSTDERR: {stderr.getvalue()}"
        finally:
            sys.argv = orig_argv
            if added_to_path:
                sys.path.remove(engine_dir)

def http_handler(event, context):
    """Triggered by Function URL for immediate task submission."""
    req_id = context.aws_request_id
    extra = {"request_id": req_id}
    
    try:
        body = json.loads(event.get('body', '{}'))
        skill = body.get('skill')
        entrypoint = body.get('entrypoint')
        command = body.get('command')
        chat_id = body.get('chat_id')
        metadata = body.get('metadata', {})
        
        extra["chat_id"] = chat_id
        
        if not all([skill, entrypoint, command, chat_id]):
            return {'statusCode': 400, 'body': json.dumps({'error': 'Missing fields (skill, entrypoint, command, chat_id)'})}

        logger.info(f"Received submission for skill '{skill}' ({entrypoint}): {command}", extra=extra)

        # Execute
        args = shlex.split(command)
        result_str, err_str = run_engine(entrypoint, args, extra)
        
        if result_str is None:
            logger.error(f"Skill submission failed: {err_str}", extra=extra)
            return {'statusCode': 500, 'body': json.dumps({'error': err_str})}

        if err_str:
            logger.info(f"Skill stderr output:\n{err_str}", extra=extra)

        # Parse Task ID and queue for monitoring
        try:
            result_json = json.loads(result_str)
            task_id = result_json.get('id')
            if task_id:
                extra["task_id"] = task_id
                engine_meta = result_json.get('_metadata', {})
                if engine_meta:
                    metadata.update(engine_meta)

                if QUEUE_URL:
                    monitor_msg = {
                        "type": "monitor_task",
                        "chat_id": chat_id,
                        "skill": skill,
                        "entrypoint": entrypoint,
                        "task_id": task_id,
                        "metadata": metadata,
                        "original_result": result_json
                    }
                    logger.info(f"Task accepted. Queuing monitor message for task_id: {task_id}", extra=extra)
                    sqs.send_message(QueueUrl=QUEUE_URL, MessageBody=json.dumps(monitor_msg), MessageGroupId=str(chat_id))
            else:
                logger.warning("Submission succeeded but no task ID found in output", extra=extra)
        except Exception as e:
            logger.warning(f"Failed to parse submission result JSON: {e}", extra=extra)

        return {'statusCode': 200, 'body': result_str}

    except Exception as e:
        logger.error(f"HTTP Handler Error: {e}", extra=extra, exc_info=True)
        return {'statusCode': 500, 'body': json.dumps({'error': str(e)})}

def sqs_handler(event, context):
    """Triggered by TaskQueue to wait for task completion."""
    req_id = context.aws_request_id
    
    for record in event.get('Records', []):
        try:
            msg = json.loads(record.get('body', '{}'))
            if msg.get('type') != 'monitor_task': continue
            
            skill = msg.get('skill')
            entrypoint = msg.get('entrypoint')
            task_id = msg.get('task_id')
            chat_id = msg.get('chat_id')
            metadata = msg.get('metadata', {})
            
            attr = record.get('attributes', {})
            retry_count = attr.get('ApproximateReceiveCount', '1')
            
            extra = {"task_id": task_id, "request_id": req_id, "chat_id": chat_id}
            logger.info(f"Checking task status (Attempt {retry_count})", extra=extra)
            
            # Execute check
            check_output, err_str = run_engine(entrypoint, ["check", "--id", task_id], extra)
            
            if check_output is None:
                logger.error(f"Status check failed: {err_str}", extra=extra)
                raise Exception(err_str)

            if err_str:
                logger.info(f"Skill stderr output:\n{err_str}", extra=extra)

            try:
                check_res = json.loads(check_output)
                done = check_res.get('done') or check_res.get('finished') == 1
                faulted = check_res.get('faulted') or check_res.get('error')
                
                if done or faulted:
                    logger.info(f"Task completed (success={done}, error={bool(faulted)}). Sending result to worker.", extra=extra)
                    if RESULT_QUEUE_URL:
                        result_msg = {
                            "type": "task_result",
                            "task_id": task_id,
                            "chat_id": chat_id,
                            "skill": skill,
                            "metadata": metadata,
                            "result": check_output,
                            "is_error": bool(faulted)
                        }
                        sqs.send_message(QueueUrl=RESULT_QUEUE_URL, MessageBody=json.dumps(result_msg), MessageGroupId=str(chat_id))
                    return
            except Exception as e:
                logger.error(f"Failed to parse engine check output: {check_output}", extra=extra)
            
            raise Exception(f"Task {task_id} not finished yet")
        
        except Exception as e:
            if "not finished yet" in str(e):
                pass
            else:
                logger.error(f"SQS Handler Error: {e}", extra=extra, exc_info=True)
            raise e
